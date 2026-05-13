package supervisor

import (
	"bytes"
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
	case req.Verb == "shutdown" || req.Verb == "update":
		// These touch every running service in the worst case.
		targets = snap
	case req.Verb == "reload" && len(req.Args) == 0:
		// No-arg reload is the alias for update.
		targets = snap
	case len(req.Args) == 0:
		// dispatch will reject with usage error; floor budget is enough.
		return minDispatchBudget
	case len(req.Args) == 1 && req.Args[0] == "all":
		targets = snap
	default:
		targets = make([]*Runner, 0, len(req.Args))
		for _, n := range req.Args {
			// Budget should reflect the actual targets; ignore
			// resolution errors here and let dispatch surface them.
			rs, err := resolveTarget(snap, n)
			if err != nil {
				continue
			}
			targets = append(targets, rs...)
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
	case "update":
		return s.cmdUpdate(ctx)
	case "reload":
		// Dual-purpose for backwards compatibility: `reload` with no
		// args remains an alias for `update` (config re-read), the
		// historical behavior. With args it performs a per-service
		// reload — the supervisord-aligned semantic — dispatching
		// reload_signal / reload_command / full restart as configured.
		if len(req.Args) == 0 {
			return s.cmdUpdate(ctx)
		}
		return s.cmdReloadService(ctx, req.Args)
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
	resp.Body = make([]string, 0, len(targets))
	var anyErr error
	for _, r := range targets {
		switch action {
		case "start":
			if err := r.StartCtx(ctx); err != nil {
				anyErr = fmt.Errorf("%s: start: %w", r.DisplayName(), err)
				resp.Body = append(resp.Body, fmt.Sprintf("%s: start failed: %v", r.DisplayName(), err))
				continue
			}
		case "stop":
			if err := r.StopCtx(ctx); err != nil {
				anyErr = fmt.Errorf("%s: stop: %w", r.DisplayName(), err)
				resp.Body = append(resp.Body, fmt.Sprintf("%s: stop failed: %v", r.DisplayName(), err))
				continue
			}
		case "restart":
			if err := r.StopCtx(ctx); err != nil {
				anyErr = fmt.Errorf("%s: restart-stop: %w", r.DisplayName(), err)
				resp.Body = append(resp.Body, fmt.Sprintf("%s: stop failed: %v", r.DisplayName(), err))
				continue
			}
			waitCtx, cancel := context.WithTimeout(ctx, r.Cfg().StopTimeout.Std()+5*time.Second)
			state, werr := r.WaitTerminal(waitCtx)
			cancel()
			if werr != nil {
				anyErr = fmt.Errorf("%s: restart-wait: %w", r.DisplayName(), werr)
				resp.Body = append(resp.Body,
					fmt.Sprintf("%s: did not stop within timeout (state=%s); restart aborted", r.DisplayName(), state))
				continue
			}
			if err := r.StartCtx(ctx); err != nil {
				anyErr = fmt.Errorf("%s: restart-start: %w", r.DisplayName(), err)
				resp.Body = append(resp.Body, fmt.Sprintf("%s: start failed: %v", r.DisplayName(), err))
				continue
			}
		}
		resp.Body = append(resp.Body, fmt.Sprintf("%s: %s", r.DisplayName(), action))
	}
	if anyErr != nil {
		resp.Code = 1
		resp.Msg = anyErr.Error()
	}
	return resp
}

// cmdReloadService implements the per-service form of `zpctl reload`.
// Targets are resolved like start/stop/restart; the orchestrator
// dispatches per-runner based on each service's reload config.
// Output mirrors cmdStartStopRestart so operators get a consistent
// "<name>: reloaded" body line per affected runner.
func (s *ControlServer) cmdReloadService(ctx context.Context, args []string) *ctlproto.Response {
	targets, err := s.expandTargets(args, false)
	if err != nil {
		return errResp(err.Error())
	}
	if err := s.orch.reloadAcrossGroups(ctx, targets); err != nil {
		resp := okResp("ok")
		for _, r := range targets {
			resp.Body = append(resp.Body, fmt.Sprintf("%s: reloaded", r.DisplayName()))
		}
		resp.Code = 1
		resp.Msg = err.Error()
		return resp
	}
	resp := okResp("ok")
	resp.Body = make([]string, 0, len(targets))
	for _, r := range targets {
		resp.Body = append(resp.Body, fmt.Sprintf("%s: reloaded", r.DisplayName()))
	}
	return resp
}

func (s *ControlServer) cmdPID(args []string) *ctlproto.Response {
	if len(args) == 0 {
		return okBody("ok", []string{strconv.Itoa(os.Getpid())})
	}
	rs, err := resolveTarget(s.orch.snapshotRunners(), args[0])
	if err != nil {
		return errResp(err.Error())
	}
	if len(rs) > 1 {
		return errResp(fmt.Sprintf("%s has %d replicas; specify which one: pid %s/N", args[0], len(rs), args[0]))
	}
	return okBody("ok", []string{strconv.Itoa(rs[0].PID())})
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
// ("will stop"), cmdUpdate uses past tense ("stopped"). Replicated
// services list each replica on its own remove line but collapse
// add/restart into a single line per service (with a "(N replicas)"
// suffix) so the operator sees the spec-level shape of the change.
func diffResp(diff reloadDiff, stopVerb, restartVerb, startVerb, emptyMsg string) *ctlproto.Response {
	resp := okResp("ok")
	for _, r := range diff.remove {
		resp.Body = append(resp.Body, fmt.Sprintf("- %s (%s)", r.DisplayName(), stopVerb))
	}
	for _, p := range diff.restart {
		resp.Body = append(resp.Body, fmt.Sprintf("~ %s (%s)", p.new.Name, restartVerb)+replicaSuffix(p.new.Replicas))
	}
	for _, svc := range diff.add {
		resp.Body = append(resp.Body, fmt.Sprintf("+ %s (%s)", svc.Name, startVerb)+replicaSuffix(svc.Replicas))
	}
	if len(resp.Body) == 0 {
		resp.Body = []string{emptyMsg}
	}
	return resp
}

// replicaSuffix renders the trailing "[N replicas]" / "[auto …]"
// tag for the reload/update diff output. Returns "" for static
// services with replicas <= 1 to keep the common case uncluttered.
func replicaSuffix(r config.Replicas) string {
	if r.Auto {
		if r.N > 0 {
			return fmt.Sprintf(" [auto, currently %d]", r.N)
		}
		return " [auto]"
	}
	if r.N <= 1 {
		return ""
	}
	return fmt.Sprintf(" [%d replicas]", r.N)
}

func (s *ControlServer) cmdTail(args []string) *ctlproto.Response {
	if len(args) != 1 {
		return errResp("usage: tail NAME[/N]")
	}
	name := args[0]
	rs, err := resolveTarget(s.orch.snapshotRunners(), name)
	if err != nil {
		return errResp(err.Error())
	}
	if len(rs) > 1 {
		return errResp(fmt.Sprintf("%s has %d replicas; specify which one: tail %s/N", name, len(rs), name))
	}
	r := rs[0]
	cfg := r.Cfg()
	if cfg.Log.Stdout == "" || cfg.Log.Stdout == "inherit" {
		return errResp(fmt.Sprintf("%s logs to stdout (no file to tail)", r.DisplayName()))
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
		return errResp("usage: signal NAME[/N] SIG")
	}
	rs, err := resolveTarget(s.orch.snapshotRunners(), args[0])
	if err != nil {
		return errResp(err.Error())
	}
	sig, ok := config.ParseSignal(args[1])
	if !ok {
		return errResp("unknown signal: " + args[1])
	}
	resp := okResp("ok")
	var anyErr error
	for _, r := range rs {
		if err := r.SignalGroup(sig); err != nil {
			anyErr = fmt.Errorf("%s: %w", r.DisplayName(), err)
			resp.Body = append(resp.Body, fmt.Sprintf("%s: %v", r.DisplayName(), err))
			continue
		}
		// Only surface per-replica lines when fanning out; the
		// single-target case keeps the historical "ok" with no body.
		if len(rs) > 1 {
			resp.Body = append(resp.Body, fmt.Sprintf("%s: signaled", r.DisplayName()))
		}
	}
	if anyErr != nil {
		resp.Code = 1
		resp.Msg = anyErr.Error()
	}
	return resp
}

func (s *ControlServer) cmdHelp() *ctlproto.Response {
	return okBody("ok", []string{
		"status [NAME...]      list service states (no args = all)",
		"start NAME[/N]...     start service(s); 'all' for everything",
		"stop NAME[/N]...      stop service(s); 'all' for everything",
		"restart NAME[/N]...   stop then start; 'all' for everything",
		"pid [NAME[/N]]        PID of zpinit (no arg) or service replica",
		"update                reload config and apply (= SIGHUP)",
		"reload                with no args: alias for update",
		"reload NAME[/N]...    in-place reload (reload_signal/_command or full restart)",
		"reread                dry-run config diff",
		"tail NAME[/N]         dump last 8KB of file-logged stdout (snapshot only)",
		"signal NAME[/N] SIG   send arbitrary signal to service's process group",
		"shutdown              stop supervisor and exit",
		"help                  this list",
		"",
		"NAME refers to a service; for services declared with replicas > 1,",
		"NAME selects every replica and NAME/N selects replica N (0..replicas-1).",
	})
}

// expandTargets resolves a list of zpctl args into the runners they
// refer to. Args may be:
//
//	"all"   - every runner (only valid as the sole arg)
//	"svc"   - every replica of svc (one runner if svc.Replicas <= 1)
//	"svc/N" - exactly replica N of svc; rejected if svc has only one
//	          replica or N is out of range
//
// Order is preserved across args; within a "svc" expansion, replicas
// appear in 0..N-1 order (the orchestrator already sorts runners that
// way).
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
		rs, err := resolveTarget(snap, n)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// resolveTarget interprets a single zpctl arg ("svc" or "svc/N")
// against the runner snapshot and returns the matching runners.
// Returns an error on unknown names or out-of-range replica indices.
func resolveTarget(snap []*Runner, arg string) ([]*Runner, error) {
	name, replicaArg, hasSlash := strings.Cut(arg, "/")
	if hasSlash {
		idx, err := strconv.Atoi(replicaArg)
		if err != nil {
			return nil, fmt.Errorf("invalid replica index %q in %q", replicaArg, arg)
		}
		for _, r := range snap {
			if r.Cfg().Name == name && r.ReplicaIndex() == idx {
				return []*Runner{r}, nil
			}
		}
		// Distinguish "unknown service name" from "name found but
		// index out of range" so operators get a useful error.
		var total int
		for _, r := range snap {
			if r.Cfg().Name == name {
				total++
			}
		}
		if total == 0 {
			return nil, fmt.Errorf("unknown service: %s", name)
		}
		return nil, fmt.Errorf("replica %d out of range for %s (has %d replica(s), valid 0..%d)", idx, name, total, total-1)
	}
	var out []*Runner
	for _, r := range snap {
		if r.Cfg().Name == name {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("unknown service: %s", arg)
	}
	return out, nil
}

func formatStatusLine(r *Runner) string {
	state := mapToSupervisordState(r.State(), r.StoppedManually())
	detail := stateDetail(r)
	return fmt.Sprintf("%-32s %-9s %s", r.DisplayName(), state, detail)
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
	// When the window starts mid-file, the first chunk is almost
	// certainly the tail of a longer line whose head is past the
	// window. Drop it so operators see whole log lines only. When
	// offset == 0 we have the whole file and trim nothing.
	if offset > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return string(buf), nil
}

func okResp(msg string) *ctlproto.Response { return &ctlproto.Response{Code: 0, Msg: msg} }

func okBody(msg string, body []string) *ctlproto.Response {
	return &ctlproto.Response{Code: 0, Msg: msg, Body: body}
}

func errResp(msg string) *ctlproto.Response { return &ctlproto.Response{Code: 1, Msg: msg} }
