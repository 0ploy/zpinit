package supervisor

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/ctlproto"
)

// fdCount returns the number of open file descriptors of this process.
// /dev/fd exists on both Linux (symlink to /proc/self/fd) and macOS.
// Readdirnames, not os.ReadDir: the latter stats every entry, which
// fails with EBADF on macOS's /dev/fd.
func fdCount(t *testing.T) int {
	t.Helper()
	d, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("cannot open /dev/fd: %v", err)
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		t.Skipf("cannot list /dev/fd: %v", err)
	}
	return len(names)
}

// TestStreamFile_RotationDoesNotLeakFD pins the deferred-close
// discipline in streamFile: the rotation branch reassigns f, and the
// deferred close must apply to the *current* file, not the one open
// when the defer was installed. A regression leaks one log FD per
// follow session that spanned a rotation.
func TestStreamFile_RotationDoesNotLeakFD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := fdCount(t)

	client, srv := net.Pipe()
	defer client.Close()
	pc := ctlproto.NewConn(srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		streamFile(ctx, srv, pc, path, testLog())
	}()

	// Drain body lines; net.Pipe is synchronous so the reader must be
	// active for streamFile's writes to complete.
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(client)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	waitLine := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream closed before %q arrived", want)
				}
				if strings.Contains(l, want) {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for line %q", want)
			}
		}
	}

	waitLine("one")

	// Rotate: rename the old file out, create a new one at the same
	// path (logrotate's default mode). The follow loop reopens on the
	// inode change and must eventually surface the new content.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitLine("two")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streamFile did not return after ctx cancel")
	}
	srv.Close()

	if after := fdCount(t); after > before {
		t.Errorf("open FDs grew across a rotated follow session: before=%d after=%d (post-rotation log FD leaked?)", before, after)
	}
}

// followHarness starts streamFile on path and returns a channel of
// received body lines plus a stop function. net.Pipe is synchronous,
// so the reader goroutine must run for streamFile's writes to land.
func followHarness(t *testing.T, path string) (<-chan string, func()) {
	t.Helper()
	client, srv := net.Pipe()
	pc := ctlproto.NewConn(srv)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		streamFile(ctx, srv, pc, path, testLog())
	}()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(client)
		sc.Buffer(make([]byte, 0, 128*1024), 128*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("streamFile did not return after ctx cancel")
		}
		srv.Close()
		client.Close()
	}
	return lines, stop
}

func waitFollowLine(t *testing.T, lines <-chan string, want string) []string {
	t.Helper()
	var seen []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("stream closed before %q arrived; saw %v", want, seen)
			}
			seen = append(seen, l)
			if strings.Contains(l, want) {
				return seen
			}
		case <-deadline:
			t.Fatalf("timed out waiting for line %q; saw %v", want, seen)
		}
	}
}

// TestStreamFile_NoTornLines pins line-based emission: a writer caught
// mid-line must not have the prefix delivered as its own body frame.
// The old byte-based drain emitted whatever was in the file at EOF, so
// "par" and "tial" arrived as two separate lines.
func TestStreamFile_NoTornLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// "par" is a partial line: its writer is mid-write.
	if err := os.WriteFile(path, []byte("one\npar"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, stop := followHarness(t, path)
	defer stop()

	seen := waitFollowLine(t, lines, "one")
	// Complete the line and expect it whole.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("tial\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	seen = append(seen, waitFollowLine(t, lines, "partial")...)
	for _, l := range seen {
		if l == "par" || l == "tial" {
			t.Fatalf("torn line fragment %q was emitted as its own frame; lines: %v", l, seen)
		}
	}
}

// TestStreamFile_OversizedLineIsChunked pins the chunking cap: a line
// longer than maxFollowChunk must arrive as several frames each below
// the client's 64KiB wire cap, instead of one giant line that makes
// the client abort with a misleading protocol failure.
func TestStreamFile_OversizedLineIsChunked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, stop := followHarness(t, path)
	defer stop()
	waitFollowLine(t, lines, "start")

	// Append the oversized line while the follow is live (the initial
	// snapshot only covers the last 8KB, so it must arrive via the
	// poll loop).
	huge := strings.Repeat("a", 3*maxFollowChunk+17)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(huge + "\nend\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	seen := waitFollowLine(t, lines, "end")
	var got strings.Builder
	for _, l := range seen[:len(seen)-1] {
		if len(l) > maxFollowChunk {
			t.Fatalf("frame of %d bytes exceeds maxFollowChunk (%d)", len(l), maxFollowChunk)
		}
		got.WriteString(l)
	}
	if got.String() != huge {
		t.Fatalf("reassembled %d bytes, want %d; chunking lost data", got.Len(), len(huge))
	}
}

// TestStreamFile_CopytruncateRestartsFromTop pins truncation handling:
// logrotate's copytruncate mode keeps the inode, so the inode-based
// rotation check never fires. The follow loop must notice the file
// shrank below its consumed offset and restart from the top instead of
// going silent forever.
func TestStreamFile_CopytruncateRestartsFromTop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, stop := followHarness(t, path)
	defer stop()

	waitFollowLine(t, lines, "two")

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("fresh\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	waitFollowLine(t, lines, "fresh")
}

// TestFollow_ClientDisconnectEndsStream pins the disconnect watcher:
// a follow client that goes away while the tailed file is idle must
// end the server-side stream promptly. Without the watcher the
// stream goroutine (and its socket + log FDs) survives until
// supervisor shutdown, because disconnect is otherwise only visible
// through a failed write and an idle file never triggers one.
func TestFollow_ClientDisconnectEndsStream(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := config.Service{
		Name: "app", Filename: "10_app.toml",
		Command: []string{"true"}, Log: config.Logging{Stdout: path},
	}
	o := &Orchestrator{log: testLog(), cfg: &config.Config{Globals: config.Globals{ExitCodeFrom: "default"}}}
	o.runners = []*Runner{NewRunner(svc, nil, 0, nil, nil, testLog())}
	s := NewControlServer(o, func() {}, testLog())

	// Not t.TempDir(): its path embeds the test name and blows past
	// the 104-char sun_path limit on macOS.
	sockDir, err := os.MkdirTemp("", "zp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "ctl.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := s.Listen(ctx, sock); err != nil {
			t.Errorf("Listen: %v", err)
		}
	}()
	// Wait for the socket to come up.
	for i := 0; ; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if i > 100 {
			t.Fatal("control socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}

	baseline := runtime.NumGoroutine()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	pc := ctlproto.NewConn(conn)
	if err := pc.WriteRequest(&ctlproto.Request{Verb: "tail", Args: []string{"--follow", "app"}}); err != nil {
		t.Fatal(err)
	}
	code, msg, err := pc.ReadStatusLine()
	if err != nil || code != 0 {
		t.Fatalf("status line: code=%d msg=%q err=%v", code, msg, err)
	}
	if line, done, err := pc.ReadBodyLine(); err != nil || done || line != "hello" {
		t.Fatalf("body line = %q done=%v err=%v, want %q", line, done, err, "hello")
	}

	// Disconnect while the file is idle. The stream goroutine and its
	// watcher must both exit; poll the goroutine count back down to
	// the pre-connection baseline.
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines never returned to baseline after client disconnect: baseline=%d now=%d (stream goroutine leaked?)",
				baseline, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
