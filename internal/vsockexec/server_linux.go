//go:build linux

// Package vsockexec is machine mode's exec listener: the microVM's
// `docker exec`, and the only way into a parked machine. It runs inside
// PID 1 as a goroutine and is opened only in machine mode with an
// `exec` block in machine.json. The wire protocol is the public package
// github.com/0ploy/zpinit/execproto.
package vsockexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/execproto"
	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/execpath"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/service"
)

// Config is the exec block of machine.json.
type Config struct {
	// User is the default exec user (the image's OCI User), used when
	// a request names none.
	User string
	// WorkingDir is the default working directory; "/" when empty.
	WorkingDir string
	// Env is the image's composed env, the base of every exec.
	Env []string
}

// Server runs exec sessions. Children go through the central reaper,
// like every other child of PID 1.
type Server struct {
	cfg    Config
	reaper *reaper.Reaper
	log    *slog.Logger
	// accept decides whether a peer CID may connect. Production
	// accepts only CIDHost.
	accept func(cid uint32) bool
}

// NewServer builds a server that accepts only the host CID.
func NewServer(cfg Config, r *reaper.Reaper, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, reaper: r, log: log, accept: func(cid uint32) bool { return cid == CIDHost }}
}

// requestTimeout bounds how long a connection may take to send its
// request, so a silent peer does not hold a goroutine forever.
const requestTimeout = 10 * time.Second

// drainGrace bounds the wait for the exec's output to reach EOF after
// it exited. A process that escaped the group (setsid) and kept the
// pipe open would otherwise hold the session forever.
const drainGrace = 2 * time.Second

// acceptor is the part of *Listener Serve uses; tests fake it.
type acceptor interface {
	Accept() (*os.File, uint32, error)
	Close() error
}

// Accept backoff when the guest is out of fds or memory, as net/http
// does: the condition is transient, and giving up would close the only
// way into the machine until it reboots.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

// Serve accepts connections until ctx is done or the listener fails.
// Only the host CID is served: vsock has no SO_PEERCRED, so the CID
// check is the whole authentication, and without it a local process
// could reach a root exec over vsock loopback.
func (s *Server) Serve(ctx context.Context, l *Listener) error {
	return s.serve(ctx, l)
}

func (s *Server) serve(ctx context.Context, l acceptor) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	var backoff time.Duration
	for {
		conn, cid, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			switch {
			case errors.Is(err, syscall.EINTR), errors.Is(err, syscall.ECONNABORTED),
				errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPROTO):
				// The pending connection went away; take the next.
				continue
			case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE),
				errors.Is(err, syscall.ENOBUFS), errors.Is(err, syscall.ENOMEM):
				backoff = min(max(2*backoff, acceptBackoffMin), acceptBackoffMax)
				s.log.Warn("exec: accept failed; retrying", "err", err, "in", backoff)
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
					return nil
				}
			}
			return err
		}
		backoff = 0
		if !s.accept(cid) {
			s.log.Warn("exec: refused connection from non-host CID", "cid", cid)
			_ = conn.Close()
			continue
		}
		go s.Handle(conn)
	}
}

// deadliner is the part of *os.File (or net.Conn) Handle uses for the
// request deadline; optional.
type deadliner interface {
	SetReadDeadline(time.Time) error
}

// Handle runs one exec session on conn and closes it.
func (s *Server) Handle(conn io.ReadWriteCloser) {
	defer conn.Close()
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("exec: session panic", "panic", p)
		}
	}()
	if d, ok := conn.(deadliner); ok {
		_ = d.SetReadDeadline(time.Now().Add(requestTimeout))
	}
	req, err := execproto.ReadRequest(conn)
	if err != nil {
		s.log.Warn("exec: bad request", "err", err)
		_ = execproto.WriteFrame(conn, execproto.FrameError, []byte(err.Error()))
		return
	}
	if d, ok := conn.(deadliner); ok {
		_ = d.SetReadDeadline(time.Time{})
	}
	sess, err := s.start(req)
	if err != nil {
		s.log.Warn("exec: start failed", "argv", req.Argv, "err", err)
		_ = execproto.WriteFrame(conn, execproto.FrameError, []byte(err.Error()))
		return
	}
	s.log.Info("exec: started", "pid", sess.pid, "argv", req.Argv, "tty", req.TTY)
	sess.run(conn)
}

// session is one running exec.
type session struct {
	log   *slog.Logger
	pid   int
	exit  <-chan reaper.ExitInfo
	tty   bool
	stdin *os.File // pipe write end, or the pty master
	outs  []output
	// master is the pty master when tty; also in outs and stdin.
	master *os.File
}

type output struct {
	f *os.File
	t execproto.FrameType
}

func (s *Server) start(req *execproto.Request) (*session, error) {
	userSpec := req.User
	if userSpec == "" {
		userSpec = s.cfg.User
	}
	id, err := service.ResolveIdentity(userSpec)
	if err != nil {
		return nil, err
	}
	dir := req.WorkingDir
	if dir == "" {
		dir = s.cfg.WorkingDir
	}
	if dir == "" {
		dir = "/"
	}
	env := composeEnv(s.cfg.Env, req, userSpec != s.cfg.User, id.Home)

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	// Resolve argv[0] against the exec's PATH, not PID 1's. A name
	// with a slash is left to the exec, relative to its working dir.
	if !strings.Contains(req.Argv[0], "/") {
		path, _ := execpath.FromEnv(env)
		p, err := execpath.Find(req.Argv[0], path)
		if err != nil {
			return nil, err
		}
		cmd.Path = p
		cmd.Err = nil
	}
	// Setsid makes the exec its own session and process group, so the
	// connection-drop SIGKILL and forwarded signals reach exactly it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// zpinit is root in production. A non-root zpinit (tests) cannot
	// switch identity, and asking for its own uid would still fail on
	// setgroups; skip the switch then.
	if euid := os.Geteuid(); euid == 0 || id.Cred.Uid != uint32(euid) {
		cmd.SysProcAttr.Credential = id.Cred
	}

	sess := &session{log: s.log, tty: req.TTY}
	var childEnds []*os.File
	closeAll := func(fs []*os.File) {
		for _, f := range fs {
			if f != nil {
				_ = f.Close()
			}
		}
	}
	if req.TTY {
		master, slave, err := openPTY(req.Rows, req.Cols)
		if err != nil {
			return nil, fmt.Errorf("allocate tty: %w", err)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr.Setctty = true
		cmd.SysProcAttr.Ctty = 0
		childEnds = []*os.File{slave}
		sess.master = master
		sess.stdin = master
		sess.outs = []output{{master, execproto.FrameStdout}}
	} else {
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			closeAll([]*os.File{inR, inW})
			return nil, err
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			closeAll([]*os.File{inR, inW, outR, outW})
			return nil, err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
		childEnds = []*os.File{inR, outW, errW}
		sess.stdin = inW
		sess.outs = []output{{outR, execproto.FrameStdout}, {errR, execproto.FrameStderr}}
	}

	proc, exitCh, err := s.reaper.SpawnTracked(func() (*os.Process, error) {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	})
	closeAll(childEnds)
	if err != nil {
		closeAll([]*os.File{sess.stdin})
		if !sess.tty {
			for _, o := range sess.outs {
				_ = o.f.Close()
			}
		}
		return nil, err
	}
	sess.pid = proc.Pid
	sess.exit = exitCh
	return sess, nil
}

// composeEnv layers the request env over the image env (docker exec
// semantics). HOME follows the exec user when the request picked a user
// other than the default; TERM is defaulted for a tty.
func composeEnv(base []string, req *execproto.Request, otherUser bool, home string) []string {
	m := map[string]string{}
	var order []string
	set := func(kv string) {
		k, v, _ := strings.Cut(kv, "=")
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = v
	}
	for _, kv := range base {
		set(kv)
	}
	if otherUser && home != "" {
		set("HOME=" + home)
	}
	if _, ok := m["HOME"]; !ok {
		h := home
		if h == "" {
			h = "/"
		}
		set("HOME=" + h)
	}
	if _, ok := m["PATH"]; !ok {
		set("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if req.TTY {
		if _, ok := m["TERM"]; !ok {
			set("TERM=xterm")
		}
	}
	for _, kv := range req.Env {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" {
			set(kv)
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}

// run pumps the session until the exec exits or the connection drops.
func (sess *session) run(conn io.ReadWriteCloser) {
	var wmu sync.Mutex
	send := func(t execproto.FrameType, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return execproto.WriteFrame(conn, t, p)
	}
	// Everything the exec started dies with it: its whole session, not
	// only its process group, because a tty shell's job control puts
	// each job in a group of its own.
	killGroup := func() { killSession(sess.pid) }

	if b, err := json.Marshal(execproto.Started{PID: sess.pid}); err == nil {
		if err := send(execproto.FrameStarted, b); err != nil {
			killGroup()
		}
	}

	// Output pumps.
	var outWG sync.WaitGroup
	for _, o := range sess.outs {
		outWG.Add(1)
		go func(o output) {
			defer outWG.Done()
			buf := make([]byte, 32*1024)
			for {
				n, err := o.f.Read(buf)
				if n > 0 {
					if serr := send(o.t, buf[:n]); serr != nil {
						killGroup()
						return
					}
				}
				if err != nil {
					if err != io.EOF && !isEIO(err) && !errors.Is(err, os.ErrClosed) {
						sess.log.Debug("exec: output read", "pid", sess.pid, "err", err)
					}
					return
				}
			}
		}(o)
	}
	outDone := make(chan struct{})
	go func() { outWG.Wait(); close(outDone) }()

	// Stdin writer: decoupled from the frame reader so a child that does
	// not read stdin cannot stall resize and signal frames.
	stdinCh := make(chan []byte, 64)
	go func() {
		for b := range stdinCh {
			if b == nil {
				if sess.tty {
					_, _ = sess.stdin.Write([]byte{eofChar})
					continue
				}
				_ = sess.stdin.Close()
				// Drain: nothing more can be written.
				for range stdinCh {
				}
				return
			}
			if _, err := sess.stdin.Write(b); err != nil && !sess.tty {
				for range stdinCh {
				}
				return
			}
		}
	}()

	// Frame reader. Any read error before the exit is a dropped
	// connection: SIGKILL the session.
	exited := make(chan struct{})
	readerDone := make(chan struct{})
	dropped := func() {
		select {
		case <-exited:
		default:
			sess.log.Info("exec: connection dropped; killing the exec's session", "pid", sess.pid)
			killGroup()
		}
	}
	// queueStdin hands p to the stdin writer, keeping backpressure: a
	// child that reads stdin slowly slows the host down rather than
	// growing a buffer in PID 1. While the queue is full the reader
	// cannot see EOF on conn, so it polls for a hangup instead; without
	// that an exec that never reads stdin would outlive a dropped
	// connection. false ends the reader.
	queueStdin := func(p []byte) bool {
		select {
		case stdinCh <- p:
			return true
		default:
		}
		tk := time.NewTicker(hangupPoll)
		defer tk.Stop()
		for {
			select {
			case stdinCh <- p:
				return true
			case <-exited:
				return false
			case <-tk.C:
				if peerHungUp(conn) {
					dropped()
					return false
				}
			}
		}
	}
	go func() {
		defer close(readerDone)
		defer close(stdinCh)
		for {
			t, p, err := execproto.ReadFrame(conn)
			if err != nil {
				dropped()
				return
			}
			switch t {
			case execproto.FrameStdin:
				if len(p) > 0 && !queueStdin(p) {
					return
				}
			case execproto.FrameStdinEOF:
				if !queueStdin(nil) {
					return
				}
			case execproto.FrameResize:
				var rz execproto.Resize
				if sess.master != nil && json.Unmarshal(p, &rz) == nil && rz.Rows > 0 && rz.Cols > 0 {
					_ = setWinsize(sess.master, rz.Rows, rz.Cols)
				}
			case execproto.FrameSignal:
				// To the exec's process group, as the protocol says:
				// a signal is the host forwarding its own, not job
				// control. Only the cleanup kill takes the session.
				if sig, ok := config.ParseSignal(string(p)); ok {
					_ = syscall.Kill(-sess.pid, sig)
				} else if n, ok := rtSignal(string(p)); ok {
					_ = syscall.Kill(-sess.pid, n)
				}
			default:
				sess.log.Warn("exec: unexpected frame from host", "type", t)
			}
		}
	}()

	info := <-sess.exit
	close(exited)
	// Whatever is left of the exec's session dies with it; output
	// already in the pipes stays readable.
	killGroup()
	select {
	case <-outDone:
	case <-time.After(drainGrace):
	}
	for _, o := range sess.outs {
		_ = o.f.Close()
	}
	<-outDone

	var ex execproto.Exit
	if info.Signaled {
		ex.Signal = config.SignalName(info.Signal)
	} else {
		code := info.ExitCode
		ex.Status = &code
	}
	if b, err := json.Marshal(ex); err == nil {
		_ = send(execproto.FrameExit, b)
	}
	sess.log.Info("exec: exited", "pid", sess.pid, "status", info.ExitCode, "signaled", info.Signaled)
	_ = conn.Close()
	<-readerDone
	if !sess.tty {
		// The stdin writer closes its end on EOF; make sure it is
		// closed when the host never sent one. This also unblocks a
		// writer parked on a pipe that an escaped process holds open.
		_ = sess.stdin.Close()
	}
}

// hangupPoll is how often a reader stalled on a full stdin queue checks
// whether the host hung up.
const hangupPoll = 250 * time.Millisecond

// peerHungUp reports whether conn's peer has closed or reset it,
// without reading (the unread bytes are stdin the child has not taken
// yet). A conn that exposes no fd never reports a hangup.
func peerHungUp(conn io.ReadWriteCloser) bool {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return true
	}
	hup := false
	if err := rc.Control(func(fd uintptr) { hup = pollHangup(int(fd)) }); err != nil {
		return true
	}
	return hup
}

// rtSignal accepts "SIG<n>" for the signals with no name (SignalName's
// output for the realtime range).
func rtSignal(s string) (syscall.Signal, bool) {
	var n int
	if _, err := fmt.Sscanf(strings.ToUpper(s), "SIG%d", &n); err != nil || n < 1 || n > 64 {
		return 0, false
	}
	return syscall.Signal(n), true
}
