//go:build linux

package vsockexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/execproto"
	"github.com/0ploy/zpinit/internal/reaper"
)

// harness runs a Server over a unix socketpair with a polling reaper,
// standing in for PID 1's SIGCHLD loop.
type harness struct {
	t      *testing.T
	srv    *Server
	client net.Conn
	done   chan struct{}
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	r := reaper.New(nil)
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				r.Reap()
			}
		}
	}()
	t.Cleanup(func() { close(stop) })

	if cfg.User == "" {
		cfg.User = strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	}
	srv := NewServer(cfg, r, nil)
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "sp")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	h := &harness{t: t, srv: srv, client: conn(fds[0]), done: make(chan struct{})}
	server := conn(fds[1])
	go func() { defer close(h.done); srv.Handle(server) }()
	t.Cleanup(func() { h.client.Close() })
	return h
}

func (h *harness) send(req *execproto.Request) {
	h.t.Helper()
	if err := execproto.WriteRequest(h.client, req); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) frame(ft execproto.FrameType, p []byte) {
	h.t.Helper()
	if err := execproto.WriteFrame(h.client, ft, p); err != nil {
		h.t.Fatal(err)
	}
}

// collect reads frames until exit/error or EOF.
type result struct {
	started *execproto.Started
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	exit    *execproto.Exit
	errMsg  string
}

func (h *harness) collect(deadline time.Duration) *result {
	h.t.Helper()
	_ = h.client.SetReadDeadline(time.Now().Add(deadline))
	res := &result{}
	for {
		ft, p, err := execproto.ReadFrame(h.client)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return res
			}
			h.t.Fatalf("read frame: %v (so far: stdout=%q stderr=%q)", err, res.stdout.String(), res.stderr.String())
		}
		switch ft {
		case execproto.FrameStarted:
			res.started = &execproto.Started{}
			_ = json.Unmarshal(p, res.started)
		case execproto.FrameStdout:
			res.stdout.Write(p)
		case execproto.FrameStderr:
			res.stderr.Write(p)
		case execproto.FrameExit:
			res.exit = &execproto.Exit{}
			if err := json.Unmarshal(p, res.exit); err != nil {
				h.t.Fatal(err)
			}
		case execproto.FrameError:
			res.errMsg = string(p)
		default:
			h.t.Fatalf("unexpected frame %v", ft)
		}
	}
}

func TestExecPipes(t *testing.T) {
	h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin", "FOO=image", "KEEP=1"}})
	h.send(&execproto.Request{V: 1, Argv: []string{"sh", "-c", `read x; echo "out:$x $FOO $KEEP"; echo err >&2; exit 3`}, Env: []string{"FOO=request"}})
	h.frame(execproto.FrameStdin, []byte("hi\n"))
	h.frame(execproto.FrameStdinEOF, nil)
	res := h.collect(10 * time.Second)
	if res.started == nil || res.started.PID <= 0 {
		t.Fatalf("no started frame: %+v", res)
	}
	if got := res.stdout.String(); got != "out:hi request 1\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := res.stderr.String(); got != "err\n" {
		t.Errorf("stderr = %q", got)
	}
	if res.exit == nil || res.exit.Status == nil || *res.exit.Status != 3 {
		t.Errorf("exit = %+v", res.exit)
	}
}

func TestExecWorkingDirAndPathLookup(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin"}, WorkingDir: "/"})
	h.send(&execproto.Request{V: 1, Argv: []string{"pwd"}, WorkingDir: dir})
	h.frame(execproto.FrameStdinEOF, nil)
	res := h.collect(10 * time.Second)
	if got := strings.TrimSpace(res.stdout.String()); got != dir {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

func TestExecTTY(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx")
	}
	h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin"}})
	h.send(&execproto.Request{V: 1, Argv: []string{"sh", "-c", `tty; stty size; echo "term=$TERM"`}, TTY: true, Rows: 30, Cols: 100})
	res := h.collect(10 * time.Second)
	out := strings.ReplaceAll(res.stdout.String(), "\r", "")
	if !strings.Contains(out, "/dev/pts/") || !strings.Contains(out, "30 100") || !strings.Contains(out, "term=xterm") {
		t.Errorf("tty output = %q", out)
	}
	if res.stderr.Len() != 0 {
		t.Errorf("stderr frames with a tty: %q", res.stderr.String())
	}
	if res.exit == nil || res.exit.Status == nil || *res.exit.Status != 0 {
		t.Errorf("exit = %+v", res.exit)
	}
}

func TestExecSignalFrame(t *testing.T) {
	h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin"}})
	h.send(&execproto.Request{V: 1, Argv: []string{"sleep", "60"}})
	// Wait for started before signalling.
	ft, _, err := execproto.ReadFrame(h.client)
	if err != nil || ft != execproto.FrameStarted {
		t.Fatalf("first frame = %v, %v", ft, err)
	}
	h.frame(execproto.FrameSignal, []byte("SIGTERM"))
	res := h.collect(10 * time.Second)
	if res.exit == nil || res.exit.Signal != "SIGTERM" {
		t.Errorf("exit = %+v", res.exit)
	}
}

func TestExecConnectionDropKillsGroup(t *testing.T) {
	h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin"}})
	// The shell forks a child in the same process group; both must die.
	h.send(&execproto.Request{V: 1, Argv: []string{"sh", "-c", `sleep 60 & echo $!; wait`}})
	_ = h.client.SetReadDeadline(time.Now().Add(10 * time.Second))
	var started execproto.Started
	var bgPID int
	for bgPID == 0 {
		ft, p, err := execproto.ReadFrame(h.client)
		if err != nil {
			t.Fatal(err)
		}
		switch ft {
		case execproto.FrameStarted:
			_ = json.Unmarshal(p, &started)
		case execproto.FrameStdout:
			bgPID, _ = strconv.Atoi(strings.TrimSpace(string(p)))
		}
	}
	h.client.Close()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end after the connection dropped")
	}
	for _, pid := range []int{started.PID, bgPID} {
		deadline := time.Now().Add(5 * time.Second)
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("pid %d survived the connection drop", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestExecErrors(t *testing.T) {
	cases := map[string]*execproto.Request{
		"version":   {V: 2, Argv: []string{"true"}},
		"no argv":   {V: 1},
		"not found": {V: 1, Argv: []string{"definitely-not-a-command-zpinit"}},
		"bad user":  {V: 1, Argv: []string{"true"}, User: "no-such-user-zpinit"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin"}})
			h.send(req)
			res := h.collect(5 * time.Second)
			if res.errMsg == "" || res.started != nil || res.exit != nil {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestComposeEnv(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/home/app", "A=1"}
	got := composeEnv(base, &execproto.Request{TTY: true, Env: []string{"A=2", "B=3"}}, false, "/root")
	want := "PATH=/bin HOME=/home/app A=2 TERM=xterm B=3"
	if strings.Join(got, " ") != want {
		t.Errorf("default user: %v, want %s", got, want)
	}
	// Another user gets its own HOME unless the request sets one.
	got = composeEnv(base, &execproto.Request{}, true, "/root")
	if strings.Join(got, " ") != "PATH=/bin HOME=/root A=1" {
		t.Errorf("other user: %v", got)
	}
	got = composeEnv(base, &execproto.Request{Env: []string{"HOME=/tmp"}}, true, "/root")
	if strings.Join(got, " ") != "PATH=/bin HOME=/tmp A=1" {
		t.Errorf("request HOME: %v", got)
	}
}

func TestAcceptOnlyHost(t *testing.T) {
	s := NewServer(Config{}, reaper.New(nil), nil)
	for cid, want := range map[uint32]bool{0: false, 1: false, CIDHost: true, 3: false, 0xFFFFFFFF: false} {
		if got := s.accept(cid); got != want {
			t.Errorf("accept(%d) = %v, want %v", cid, got, want)
		}
	}
}

// readStarted reads frames up to the started frame and returns its pid.
func (h *harness) readStarted() int {
	h.t.Helper()
	_ = h.client.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		ft, p, err := execproto.ReadFrame(h.client)
		if err != nil {
			h.t.Fatal(err)
		}
		if ft == execproto.FrameStarted {
			var st execproto.Started
			_ = json.Unmarshal(p, &st)
			_ = h.client.SetReadDeadline(time.Time{})
			return st.PID
		}
	}
}

// floodStdin writes stdin frames until the connection fails.
func (h *harness) floodStdin() {
	go func() {
		chunk := bytes.Repeat([]byte("x"), 32*1024)
		for execproto.WriteFrame(h.client, execproto.FrameStdin, chunk) == nil {
		}
	}()
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return
		}
		if st, _, ok := statSession(b); ok && st == 'Z' {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d survived", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An exec that never reads stdin fills the stdin queue and stalls the
// frame reader. A connection dropped then must still be noticed and
// the exec killed.
func TestExecDropWhileStdinStalled(t *testing.T) {
	h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin"}})
	h.send(&execproto.Request{V: 1, Argv: []string{"sleep", "60"}})
	pid := h.readStarted()
	h.floodStdin()
	// Long enough for the pipe and the queue to fill.
	time.Sleep(500 * time.Millisecond)
	h.client.Close()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end after the connection dropped with stdin stalled")
	}
	waitGone(t, pid)
}

// An escaped process holding the stdin pipe keeps the stdin writer
// blocked; the session must still end when the exec exits.
func TestExecExitWhileStdinStalled(t *testing.T) {
	h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin"}})
	h.send(&execproto.Request{V: 1, Argv: []string{"sh", "-c", `exec 3<&0; setsid sleep 60 <&3 3<&- >/dev/null 2>&1 & echo $!; sleep 1; exit 0`}})
	h.readStarted()
	var esc int
	_ = h.client.SetReadDeadline(time.Now().Add(10 * time.Second))
	for esc == 0 {
		ft, p, err := execproto.ReadFrame(h.client)
		if err != nil {
			t.Fatal(err)
		}
		if ft == execproto.FrameStdout {
			esc, _ = strconv.Atoi(strings.TrimSpace(string(p)))
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(esc, syscall.SIGKILL) })
	h.floodStdin()
	select {
	case <-h.done:
	case <-time.After(15 * time.Second):
		t.Fatal("session did not end after the exec exited with stdin stalled")
	}
}

// A tty shell with job control puts each job in its own process group.
// The connection-drop kill must reach them too: it takes the session.
func TestExecConnectionDropKillsSession(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx")
	}
	h := newHarness(t, Config{Env: []string{"PATH=/usr/bin:/bin"}})
	h.send(&execproto.Request{V: 1, Argv: []string{"sh", "-c", `set -m; sleep 60 & echo "bg=$!"; wait`}, TTY: true})
	leader := h.readStarted()
	var bg int
	var out strings.Builder
	_ = h.client.SetReadDeadline(time.Now().Add(10 * time.Second))
	for bg == 0 {
		ft, p, err := execproto.ReadFrame(h.client)
		if err != nil {
			t.Fatalf("%v (output %q)", err, out.String())
		}
		if ft == execproto.FrameStdout {
			out.Write(p)
			if _, v, ok := strings.Cut(out.String(), "bg="); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && strings.ContainsAny(v, "\r\n") {
					bg = n
				}
			}
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(bg, syscall.SIGKILL) })
	// The scenario only exists if the job really left the group.
	if pgid, err := syscall.Getpgid(bg); err != nil || pgid == leader {
		t.Skipf("the shell did not move the job to its own group (pgid %d, err %v)", pgid, err)
	}
	h.client.Close()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end after the connection dropped")
	}
	waitGone(t, leader)
	waitGone(t, bg)
}

func TestStatSession(t *testing.T) {
	st, sid, ok := statSession([]byte("4242 (a) b (c) S 1 4242 4300 0 -1 4194560\n"))
	if !ok || st != 'S' || sid != 4300 {
		t.Errorf("statSession = %c %d %v", st, sid, ok)
	}
	if _, _, ok := statSession([]byte("garbage")); ok {
		t.Error("statSession(garbage) ok")
	}
}

// fakeAcceptor fails with errs in turn, then blocks until closed.
type fakeAcceptor struct {
	errs   []error
	calls  chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (f *fakeAcceptor) Accept() (*os.File, uint32, error) {
	f.calls <- struct{}{}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, 0, err
	}
	<-f.closed
	return nil, 0, os.ErrClosed
}

func (f *fakeAcceptor) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// Running out of fds or memory is transient: the listener must back
// off and keep accepting, not close for the life of the machine.
func TestServeSurvivesTransientAcceptErrors(t *testing.T) {
	s := NewServer(Config{}, reaper.New(nil), nil)
	f := &fakeAcceptor{
		errs:   []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED},
		calls:  make(chan struct{}, 16),
		closed: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	accepts := len(f.errs) + 1 // read before serve starts consuming errs
	go func() { done <- s.serve(ctx, f) }()
	for i := range accepts {
		select {
		case <-f.calls:
		case err := <-done:
			t.Fatalf("serve returned after %d accepts: %v", i, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("accept %d never came", i+1)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve after cancel = %v", err)
	}
}

// A permanent error still ends Serve, so the caller logs it.
func TestServeReturnsPermanentAcceptError(t *testing.T) {
	s := NewServer(Config{}, reaper.New(nil), nil)
	f := &fakeAcceptor{errs: []error{syscall.EBADF}, calls: make(chan struct{}, 4), closed: make(chan struct{})}
	if err := s.serve(context.Background(), f); !errors.Is(err, syscall.EBADF) {
		t.Errorf("serve = %v, want EBADF", err)
	}
}
