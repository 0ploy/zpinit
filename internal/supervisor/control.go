package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/ctlproto"
)

// ControlServer is the daemon side of the zpctl protocol. One request
// per connection, dispatched against the orchestrator's runners.
type ControlServer struct {
	orch     *Orchestrator
	shutdown func() // called by `zpctl shutdown` to trigger orderly exit
	log      *slog.Logger
}

// readRequestTimeout caps how long a client may take to send the
// request line. Bounded so a slow client can't pin a handler
// goroutine waiting on the read.
const readRequestTimeout = 5 * time.Second

// minDispatchBudget is the floor for the post-request connection
// deadline — covers status/pid/help/etc. that don't touch the
// runners. Stop-driven verbs add per-target stop_timeout on top via
// dispatchBudget.
const minDispatchBudget = 30 * time.Second

// NewControlServer wires the server to an orchestrator. shutdownFn
// should cancel whatever context Orchestrator.Run is parked on.
func NewControlServer(orch *Orchestrator, shutdownFn func(), log *slog.Logger) *ControlServer {
	if log == nil {
		log = slog.Default()
	}
	return &ControlServer{orch: orch, shutdown: shutdownFn, log: log}
}

// Listen binds a Unix socket at path (mode 0600) and serves until ctx
// is canceled. Removes any stale socket file before binding.
//
// Umask is tightened to 0o077 across the bind so the socket is born
// 0700 from the kernel's perspective; without that, the bind creates
// the file as 0777&~umask (typically 0755) and a non-root local
// process can connect during the window between bind and the chmod
// below. Umask is process-global, but at this point in startup no
// other goroutine is creating files (entrypoint.d already finished;
// runner goroutines are spawned later by the orchestrator), so the
// flip is safe. The chmod is kept as belt-and-braces.
func (s *ControlServer) Listen(ctx context.Context, path string) error {
	_ = os.Remove(path)
	old := syscall.Umask(0o077)
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}

	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log.Warn("control accept", "err", err)
			// Back off briefly so a persistent error (EMFILE etc.)
			// doesn't busy-loop and flood logs.
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *ControlServer) handleConn(conn net.Conn) {
	defer conn.Close()
	// Recover from any panic in dispatch — without this a single bad
	// request crashes PID 1 and takes the container down. Mirrors
	// safeReap in cmd/zpinit/main.go.
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("control dispatch panic; connection dropped", "panic", p)
		}
	}()

	// Defense in depth: filesystem perms (0600) are the first gate;
	// SO_PEERCRED is the second. A peer with a different UID than the
	// daemon is rejected without dispatch — covers the (narrow) case
	// where someone slipped in an FD before the chmod and then forked
	// across a privilege boundary, plus any future relaxation of the
	// socket mode. No-op on non-Linux; zpinit only runs production on
	// Linux.
	if err := authorizePeer(conn); err != nil {
		s.log.Warn("control: peer rejected", "err", err)
		return
	}

	// Two-phase deadline: a short read budget for the request line, then
	// a verb-aware budget for the dispatch + write. The old single-60s
	// deadline expired mid-loop on `restart all` for many services,
	// leaving operator and PID-1 state inconsistent.
	_ = conn.SetReadDeadline(time.Now().Add(readRequestTimeout))
	pc := ctlproto.NewConn(conn)
	req, err := pc.ReadRequest()
	if err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(readRequestTimeout))
		_ = pc.WriteResponse(&ctlproto.Response{Code: 1, Msg: "bad request: " + err.Error()})
		return
	}

	deadline := time.Now().Add(s.dispatchBudget(req))
	_ = conn.SetDeadline(deadline)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	resp := s.dispatch(ctx, req)
	_ = pc.WriteResponse(resp)
}

// dispatchBudget returns how long the connection may stay open for
// this verb's work. Verbs that drive Stop on services need to cover
// sum-of-(stop_timeout+grace) over the affected runners; everything
// else gets the floor.
func (s *ControlServer) dispatchBudget(req *ctlproto.Request) time.Duration {
	switch req.Verb {
	case "stop", "restart", "update", "reload", "shutdown":
	default:
		return minDispatchBudget
	}

	snap := s.orch.snapshotRunners()
	var targets []*Runner
	switch {
	case req.Verb == "shutdown" || req.Verb == "update" || req.Verb == "reload":
		// These touch every running service in the worst case.
		targets = snap
	case len(req.Args) == 0:
		// dispatch will reject with usage error; floor budget is enough.
		return minDispatchBudget
	case len(req.Args) == 1 && req.Args[0] == "all":
		targets = snap
	default:
		targets = make([]*Runner, 0, len(req.Args))
		for _, n := range req.Args {
			for _, r := range snap {
				if r.Cfg().Name == n {
					targets = append(targets, r)
					break
				}
			}
		}
	}

	const perTargetGrace = 10 * time.Second
	budget := minDispatchBudget
	for _, r := range targets {
		budget += r.Cfg().StopTimeout.Std() + perTargetGrace
	}
	return budget
}

func (s *ControlServer) dispatch(ctx context.Context, req *ctlproto.Request) *ctlproto.Response {
	switch req.Verb {
	case "status":
		return s.cmdStatus(req.Args)
	case "start":
		return s.cmdStartStopRestart(ctx, req.Args, "start")
	case "stop":
		return s.cmdStartStopRestart(ctx, req.Args, "stop")
	case "restart":
		return s.cmdStartStopRestart(ctx, req.Args, "restart")
	case "pid":
		return s.cmdPID(req.Args)
	case "update", "reload":
		return s.cmdUpdate(ctx)
	case "reread":
		return s.cmdReread()
	case "tail":
		return s.cmdTail(req.Args)
	case "shutdown":
		return s.cmdShutdown()
	case "signal":
		return s.cmdSignal(req.Args)
	case "help":
		return s.cmdHelp()
	default:
		return errResp("unknown command: " + req.Verb)
	}
}

func (s *ControlServer) cmdStatus(args []string) *ctlproto.Response {
	targets, err := s.expandTargets(args, true)
	if err != nil {
		return errResp(err.Error())
	}
	resp := okResp("ok")
	for _, r := range targets {
		resp.Body = append(resp.Body, formatStatusLine(r))
	}
	return resp
}

func (s *ControlServer) cmdStartStopRestart(ctx context.Context, args []string, action string) *ctlproto.Response {
	if len(args) == 0 {
		return errResp(fmt.Sprintf("usage: %s NAME [NAME...] | %s all", action, action))
	}
	targets, err := s.expandTargets(args, false)
	if err != nil {
		return errResp(err.Error())
	}
	resp := okResp("ok")
	var anyErr error
	for _, r := range targets {
		switch action {
		case "start":
			if err := r.StartCtx(ctx); err != nil {
				anyErr = fmt.Errorf("%s: start: %w", r.Cfg().Name, err)
				resp.Body = append(resp.Body, fmt.Sprintf("%s: start failed: %v", r.Cfg().Name, err))
				continue
			}
		case "stop":
			if err := r.StopCtx(ctx); err != nil {
				anyErr = fmt.Errorf("%s: stop: %w", r.Cfg().Name, err)
				resp.Body = append(resp.Body, fmt.Sprintf("%s: stop failed: %v", r.Cfg().Name, err))
				continue
			}
		case "restart":
			if err := r.StopCtx(ctx); err != nil {
				anyErr = fmt.Errorf("%s: restart-stop: %w", r.Cfg().Name, err)
				resp.Body = append(resp.Body, fmt.Sprintf("%s: stop failed: %v", r.Cfg().Name, err))
				continue
			}
			waitCtx, cancel := context.WithTimeout(ctx, r.Cfg().StopTimeout.Std()+5*time.Second)
			state, werr := r.WaitTerminal(waitCtx)
			cancel()
			if werr != nil {
				anyErr = fmt.Errorf("%s: restart-wait: %w", r.Cfg().Name, werr)
				resp.Body = append(resp.Body,
					fmt.Sprintf("%s: did not stop within timeout (state=%s); restart aborted", r.Cfg().Name, state))
				continue
			}
			if err := r.StartCtx(ctx); err != nil {
				anyErr = fmt.Errorf("%s: restart-start: %w", r.Cfg().Name, err)
				resp.Body = append(resp.Body, fmt.Sprintf("%s: start failed: %v", r.Cfg().Name, err))
				continue
			}
		}
		resp.Body = append(resp.Body, fmt.Sprintf("%s: %s", r.Cfg().Name, action))
	}
	if anyErr != nil {
		resp.Code = 1
		resp.Msg = anyErr.Error()
	}
	return resp
}

func (s *ControlServer) cmdPID(args []string) *ctlproto.Response {
	if len(args) == 0 {
		return okBody("ok", []string{strconv.Itoa(os.Getpid())})
	}
	r := s.orch.findRunner(args[0])
	if r == nil {
		return errResp("unknown service: " + args[0])
	}
	return okBody("ok", []string{strconv.Itoa(r.PID())})
}

func (s *ControlServer) cmdUpdate(ctx context.Context) *ctlproto.Response {
	newCfg, err := config.Load(s.orch.configDir())
	if err != nil {
		return errResp("load: " + err.Error())
	}
	// Snapshot the diff before Reload applies it so the client sees
	// exactly what was acted on. The window between this computeDiff
	// and Reload's internal computeDiffLocked is the same window
	// cmdReread already has against any subsequent Reload — i.e.
	// vanishingly small and benign.
	diff := s.orch.computeDiff(newCfg)
	if err := s.orch.Reload(ctx, newCfg); err != nil {
		return errResp("reload: " + err.Error())
	}
	return diffResp(diff, "stopped", "restarted", "started", "no changes")
}

func (s *ControlServer) cmdReread() *ctlproto.Response {
	newCfg, err := config.Load(s.orch.configDir())
	if err != nil {
		return errResp("load: " + err.Error())
	}
	diff := s.orch.computeDiff(newCfg)
	return diffResp(diff, "will stop", "will restart", "will start", "no changes")
}

// diffResp renders a reload diff into a response body using
// caller-supplied verb phrases. cmdReread uses future tense
// ("will stop"), cmdUpdate uses past tense ("stopped").
func diffResp(diff reloadDiff, stopVerb, restartVerb, startVerb, emptyMsg string) *ctlproto.Response {
	resp := okResp("ok")
	for _, r := range diff.remove {
		resp.Body = append(resp.Body, fmt.Sprintf("- %s (%s)", r.Cfg().Name, stopVerb))
	}
	for _, p := range diff.restart {
		resp.Body = append(resp.Body, fmt.Sprintf("~ %s (%s)", p.new.Name, restartVerb))
	}
	for _, svc := range diff.add {
		resp.Body = append(resp.Body, fmt.Sprintf("+ %s (%s)", svc.Name, startVerb))
	}
	if len(resp.Body) == 0 {
		resp.Body = []string{emptyMsg}
	}
	return resp
}

func (s *ControlServer) cmdTail(args []string) *ctlproto.Response {
	if len(args) != 1 {
		return errResp("usage: tail NAME")
	}
	name := args[0]
	r := s.orch.findRunner(name)
	if r == nil {
		return errResp("unknown service: " + name)
	}
	cfg := r.Cfg()
	if cfg.Log.Stdout == "" || cfg.Log.Stdout == "inherit" {
		return errResp(fmt.Sprintf("%s logs to stdout (no file to tail)", name))
	}
	body, err := readLastBytes(cfg.Log.Stdout, 8192)
	if err != nil {
		return errResp(err.Error())
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	return okBody("ok", lines)
}

func (s *ControlServer) cmdShutdown() *ctlproto.Response {
	if s.shutdown != nil {
		go s.shutdown()
	}
	return okResp("shutting down")
}

func (s *ControlServer) cmdSignal(args []string) *ctlproto.Response {
	if len(args) != 2 {
		return errResp("usage: signal NAME SIG")
	}
	r := s.orch.findRunner(args[0])
	if r == nil {
		return errResp("unknown service: " + args[0])
	}
	sig, ok := config.ParseSignal(args[1])
	if !ok {
		return errResp("unknown signal: " + args[1])
	}
	if err := r.SignalGroup(sig); err != nil {
		return errResp(err.Error())
	}
	return okResp("ok")
}

func (s *ControlServer) cmdHelp() *ctlproto.Response {
	return okBody("ok", []string{
		"status [NAME...]    list service states (no args = all)",
		"start NAME...       start service(s); 'all' for everything",
		"stop NAME...        stop service(s); 'all' for everything",
		"restart NAME...     stop then start; 'all' for everything",
		"pid [NAME]          PID of zpinit (no arg) or service",
		"update              reload config and apply (= SIGHUP)",
		"reload              alias for update (note: differs from supervisord's reload)",
		"reread              dry-run config diff",
		"tail NAME           dump last 8KB of file-logged stdout (snapshot only)",
		"signal NAME SIG     send arbitrary signal to service's process group",
		"shutdown            stop supervisor and exit",
		"help                this list",
	})
}

func (s *ControlServer) expandTargets(args []string, allOnEmpty bool) ([]*Runner, error) {
	snap := s.orch.snapshotRunners()
	if len(args) == 0 {
		if allOnEmpty {
			return snap, nil
		}
		return nil, fmt.Errorf("no service named")
	}
	if len(args) == 1 && args[0] == "all" {
		return snap, nil
	}
	out := make([]*Runner, 0, len(args))
	for _, n := range args {
		var found *Runner
		for _, r := range snap {
			if r.Cfg().Name == n {
				found = r
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("unknown service: %s", n)
		}
		out = append(out, found)
	}
	return out, nil
}

func formatStatusLine(r *Runner) string {
	cfg := r.Cfg()
	state := mapToSupervisordState(r.State(), r.StoppedManually())
	detail := stateDetail(r)
	return fmt.Sprintf("%-32s %-9s %s", cfg.Name, state, detail)
}

func mapToSupervisordState(s State, manualStop bool) string {
	switch s {
	case StatePending:
		return "STOPPED"
	case StateStarting:
		return "STARTING"
	case StateRunning:
		return "RUNNING"
	case StateBackoff:
		return "BACKOFF"
	case StateStopping:
		return "STOPPING"
	case StateStopped:
		if manualStop {
			return "STOPPED"
		}
		return "EXITED"
	case StateFatal:
		return "FATAL"
	}
	return "UNKNOWN"
}

func stateDetail(r *Runner) string {
	switch r.State() {
	case StateRunning:
		up := r.UpSince()
		if up.IsZero() {
			return fmt.Sprintf("pid %d", r.PID())
		}
		return fmt.Sprintf("pid %d, uptime %s", r.PID(), formatUptime(time.Since(up)))
	case StateBackoff:
		return fmt.Sprintf("backoff (crashes %d)", r.Crashes())
	case StateFatal:
		return "Exited too quickly (process log may have details)"
	case StateStarting:
		return "starting"
	case StateStopping:
		return "stopping"
	}
	return ""
}

// formatUptime renders a duration as H:MM:SS (or Dd HH:MM if it's been
// up for more than a day), matching supervisorctl status output.
func formatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSecs := int(d.Seconds())
	days := totalSecs / 86400
	hours := (totalSecs % 86400) / 3600
	mins := (totalSecs % 3600) / 60
	secs := totalSecs % 60
	if days > 0 {
		return fmt.Sprintf("%dd %02d:%02d", days, hours, mins)
	}
	return fmt.Sprintf("%d:%02d:%02d", hours, mins, secs)
}

func readLastBytes(path string, n int64) (string, error) {
	// O_NOFOLLOW rejects the open if the final path component is a
	// symlink, so a service config that points log.stdout at a
	// symlink can't trick `zpctl tail` into reading whatever the
	// link targets. The IsRegular check below covers the rest:
	// device files, FIFOs, and directories all return non-regular
	// from Stat and would either hang or dump nonsense.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	offset := st.Size() - n
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return "", err
	}
	buf := make([]byte, st.Size()-offset)
	if _, err := io.ReadFull(f, buf); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	return string(buf), nil
}

func okResp(msg string) *ctlproto.Response { return &ctlproto.Response{Code: 0, Msg: msg} }

func okBody(msg string, body []string) *ctlproto.Response {
	return &ctlproto.Response{Code: 0, Msg: msg, Body: body}
}

func errResp(msg string) *ctlproto.Response { return &ctlproto.Response{Code: 1, Msg: msg} }
