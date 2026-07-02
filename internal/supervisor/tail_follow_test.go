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
