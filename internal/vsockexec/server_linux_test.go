//go:build linux

package vsockexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
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
