package supervisor

import (
	"bufio"
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
	"sync"
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

	// Streaming verbs (`tail --follow`) bypass the regular Response
	// path: the handler writes its own status line, streams body
	// lines as they arrive, and writes the terminator on shutdown.
	// We hand it the live conn so it can manage its own write
	// deadline (a regular dispatch budget is meaningless for an
	// open-ended follow). Read deadline is cleared because there's
	// no further client-driven I/O to time-bound; the goroutine
	// exits when the client closes the connection (write error) or
	// the supervisor shuts down.
	if isStreamingRequest(req) {
		_ = conn.SetReadDeadline(time.Time{})
		streamCtx, streamCancel := context.WithCancel(context.Background())
		defer streamCancel()
		s.handleStream(streamCtx, conn, pc, req)
		return
	}

	deadline := time.Now().Add(s.dispatchBudget(req))
	_ = conn.SetDeadline(deadline)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	resp := s.dispatch(ctx, req)
	_ = pc.WriteResponse(resp)
}

// isStreamingRequest reports whether the request takes the
// streaming code path (status line + ad-hoc body lines + late
// terminator). Today only `tail --follow` qualifies.
func isStreamingRequest(req *ctlproto.Request) bool {
	if req.Verb != "tail" {
		return false
	}
	for _, a := range req.Args {
		if a == "--follow" || a == "-f" {
			return true
		}
	}
	return false
}

// handleStream dispatches the streaming verbs and writes the
// response terminator on the way out. The verb handlers stream
// body lines directly via pc.WriteBodyLine and return when they're
// done (client disconnect, ctx cancel, or self-termination).
func (s *ControlServer) handleStream(ctx context.Context, conn net.Conn, pc *ctlproto.Conn, req *ctlproto.Request) {
	switch req.Verb {
	case "tail":
		s.cmdTailFollow(ctx, conn, pc, req.Args)
	default:
		_ = pc.WriteResponse(&ctlproto.Response{Code: 1, Msg: "internal: unknown streaming verb " + req.Verb})
		return
	}
	// Terminator after the stream ends. Best-effort: the client may
	// already be gone, in which case the write fails and we log
	// nothing.
	_ = pc.WriteEnd()
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

	// Budget per filename group, not per runner. dispatch processes
	// replicas of one logical service in parallel (matching stopAll
	// / removeServiceGroup), so a `replicas = 64` service contributes
	// one unit of stop_timeout+grace to the budget, not 64. The
	// previous per-runner accumulation produced multi-hour deadlines
	// for `stop all` / `restart all` on heavily-replicated services,
	// which kept the daemon-side handler goroutine and socket open
	// for that long even after a client disconnect.
	//
	// Per-verb shape: `reload` with reload_signal configured is a
	// kill(2) and finishes in microseconds; no stop budget needed.
	// `reload` with reload_command waits at most reloadCommandTimeout.
	// Everything else (stop, restart, reload-as-restart, update,
	// shutdown) needs stop_timeout + grace per filename group.
	const perTargetGrace = reapGrace + 5*time.Second
	budget := minDispatchBudget
	seenFiles := make(map[string]struct{}, len(targets))
	for _, r := range targets {
		cfg := r.Cfg()
		fn := cfg.Filename
		if _, ok := seenFiles[fn]; ok {
			continue
		}
		seenFiles[fn] = struct{}{}
		if req.Verb == "reload" {
			if cfg.ReloadSignal != "" {
				continue
			}
			if len(cfg.ReloadCommand) > 0 {
				budget += reloadCommandTimeout
				continue
			}
		}
		budget += cfg.StopTimeout.Std() + perTargetGrace
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
	case "ready":
		return s.cmdReady(req.Args)
	case "help":
		return s.cmdHelp()
	default:
		return errResp("unknown command: " + req.Verb)
	}
}

func (s *ControlServer) cmdStatus(args []string) *ctlproto.Response {
	// --verbose appended to status decorates each runner line with
	// /proc-derived data (RSS, CPU time, fd count) and lifetime
	// counters (last exit, total spawns). Stripped before passing to
	// expandTargets so service-name parsing stays simple. Anywhere in
	// the arg list works ("status --verbose all", "status all
	// --verbose") so operators don't have to remember the position.
	verbose, args := extractFlag(args, "--verbose")
	targets, err := s.expandTargets(args, true)
	if err != nil {
		return errResp(err.Error())
	}
	resp := okResp("ok")
	for _, r := range targets {
		if verbose {
			resp.Body = append(resp.Body, formatStatusLineVerbose(r))
		} else {
			resp.Body = append(resp.Body, formatStatusLine(r))
		}
	}
	return resp
}

// extractFlag removes every occurrence of name from args and reports
// whether it was present. Used to strip control-protocol flags
// before name resolution. Multi-strip is intentional so operators
// don't get confusing "unknown service: --verbose" errors when they
// accidentally write the flag twice.
func extractFlag(args []string, name string) (bool, []string) {
	found := false
	out := args[:0]
	for _, a := range args {
		if a == name {
			found = true
			continue
		}
		out = append(out, a)
	}
	return found, out
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
	var collected []error

	// Group consecutive same-filename targets and dispatch each group
	// in parallel, mirroring stopAll's parallel-within-group / serial-
	// between-groups schedule. expandTargets returns replicas of one
	// service consecutively (resolveTarget walks them in 0..N-1 order),
	// so simple linear grouping suffices. Without this, `zpctl restart
	// all` on a service with replicas = 64 took ≈ 64 × stop_timeout
	// sequentially even though SIGTERM-driven shutdown finishes in one
	// stop_timeout.
	for i := 0; i < len(targets); {
		fn := targets[i].Cfg().Filename
		j := i
		for j < len(targets) && targets[j].Cfg().Filename == fn {
			j++
		}
		group := targets[i:j]
		i = j

		bodies := make([]string, len(group))
		errs := make([]error, len(group))
		var wg sync.WaitGroup
		for k, r := range group {
			wg.Add(1)
			go func(k int, r *Runner) {
				defer wg.Done()
				name := r.DisplayName()
				switch action {
				case "start":
					if err := r.StartCtx(ctx); err != nil {
						errs[k] = fmt.Errorf("%s: start: %w", name, err)
						bodies[k] = fmt.Sprintf("%s: start failed: %v", name, err)
						return
					}
				case "stop":
					if err := r.StopCtx(ctx); err != nil {
						errs[k] = fmt.Errorf("%s: stop: %w", name, err)
						bodies[k] = fmt.Sprintf("%s: stop failed: %v", name, err)
						return
					}
				case "restart":
					if err := r.StopCtx(ctx); err != nil {
						errs[k] = fmt.Errorf("%s: restart-stop: %w", name, err)
						bodies[k] = fmt.Sprintf("%s: stop failed: %v", name, err)
						return
					}
					waitCtx, cancel := context.WithTimeout(ctx, r.Cfg().StopTimeout.Std()+reapGrace)
					state, werr := r.WaitTerminal(waitCtx)
					cancel()
					if werr != nil {
						errs[k] = fmt.Errorf("%s: restart-wait: %w", name, werr)
						bodies[k] = fmt.Sprintf("%s: did not stop within timeout (state=%s); restart aborted", name, state)
						return
					}
					if err := r.StartCtx(ctx); err != nil {
						errs[k] = fmt.Errorf("%s: restart-start: %w", name, err)
						bodies[k] = fmt.Sprintf("%s: start failed: %v", name, err)
						return
					}
				}
				bodies[k] = fmt.Sprintf("%s: %s", name, action)
			}(k, r)
		}
		wg.Wait()
		for k := range group {
			resp.Body = append(resp.Body, bodies[k])
			if errs[k] != nil {
				collected = append(collected, errs[k])
			}
		}
	}

	// errors.Join keeps every per-target failure in the status-line
	// message, sanitized to one wire line by WriteResponse. The
	// previous "last error wins" behavior dropped N-1 failures when
	// `restart svcA svcB svcC` had multiple problems, leaving the
	// operator with only the alphabetically-last service's error.
	if len(collected) > 0 {
		resp.Code = 1
		resp.Msg = errors.Join(collected...).Error()
	}
	return resp
}

// cmdReloadService implements the per-service form of `zpctl reload`.
// Targets are resolved like start/stop/restart; the orchestrator
// dispatches per-runner based on each service's reload config.
//
// The body line per target reflects the actual outcome:
//
//	"svc: reloaded"                                   success
//	"svc: reload_command exited 1 (service still running)"   reload_command path
//	"svc: signal: <err>"                              reload_signal could not deliver
//
// so an operator (or a CI step running `zpctl reload nginx`) gets a
// fail-closed signal when a reload_command misfires even though the
// supervised process itself is unaffected.
func (s *ControlServer) cmdReloadService(ctx context.Context, args []string) *ctlproto.Response {
	targets, err := s.expandTargets(args, false)
	if err != nil {
		return errResp(err.Error())
	}
	perTarget, aggErr := s.orch.reloadAcrossGroups(ctx, targets)
	resp := okResp("ok")
	resp.Body = make([]string, 0, len(targets))
	for i, r := range targets {
		var line string
		if i < len(perTarget) && perTarget[i] != nil {
			line = fmt.Sprintf("%s: %v", r.DisplayName(), perTarget[i])
		} else {
			line = fmt.Sprintf("%s: reloaded", r.DisplayName())
		}
		resp.Body = append(resp.Body, line)
	}
	if aggErr != nil {
		resp.Code = 1
		resp.Msg = aggErr.Error()
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
	// Reload returns the diff it actually applied, so the response
	// shape matches what landed even if another reload races between
	// our load and Reload's internal computeDiffLocked. The previous
	// two-walk approach (computeDiff here, computeDiffLocked inside
	// Reload) could disagree under contention. On reload error the
	// diff still reflects the attempted action; the runner registry
	// retries failed entries on the next reload.
	diff, rerr := s.orch.Reload(ctx, newCfg)
	if rerr != nil {
		return errResp("reload: " + rerr.Error())
	}
	return diffResp(diff, "stopped", "restarted", "started", "no changes")
}

func (s *ControlServer) cmdReread() *ctlproto.Response {
	newCfg, err := config.Load(s.orch.configDir())
	if err != nil {
		return errResp("load: " + err.Error())
	}
	// Advisory dry-run: computeDiff takes o.mu but not reloadMu, so a
	// Reload arriving between the diff and the operator's reaction
	// can change the answer. That is acceptable — `reread` is a
	// snapshot, not a transaction; the next `update` recomputes.
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

// cmdTailFollow streams new lines as they're appended to a
// service's stdout log file until the client disconnects or the
// supervisor shuts down. Polls with os.Stat + ReadAt rather than
// inotify so it works on every container filesystem (tmpfs,
// overlayfs, NFS — inotify is famously unreliable on the second
// and third).
//
// Detects log rotation by inode change (logrotate's default mode
// renames the old file and creates a new one). When the inode
// moves, the next poll reopens the new file from offset 0 so the
// operator's view follows the rotation instead of getting wedged
// on a file that no app writes to anymore.
//
// Wire shape: writes the status line "0 ok" immediately, then
// streams one body line per log line (after sanitization). The
// terminator is written by handleStream when this function
// returns; the client's read loop treats the terminator (or a
// network error) as the end of the stream.
func (s *ControlServer) cmdTailFollow(ctx context.Context, conn net.Conn, pc *ctlproto.Conn, args []string) {
	// Args layout: ["--follow", "name"] or ["name", "--follow"], in
	// either order. Strip the flag (and any -f alias) before name
	// resolution.
	_, args = extractFlag(args, "--follow")
	_, args = extractFlag(args, "-f")
	if len(args) != 1 {
		_ = pc.WriteStatusLine(1, "usage: tail --follow NAME[/N]")
		return
	}
	name := args[0]
	rs, err := resolveTarget(s.orch.snapshotRunners(), name)
	if err != nil {
		_ = pc.WriteStatusLine(1, err.Error())
		return
	}
	if len(rs) > 1 {
		_ = pc.WriteStatusLine(1, fmt.Sprintf("%s has %d replicas; specify which one: tail --follow %s/N", name, len(rs), name))
		return
	}
	cfg := rs[0].Cfg()
	if cfg.Log.Stdout == "" || cfg.Log.Stdout == "inherit" {
		_ = pc.WriteStatusLine(1, fmt.Sprintf("%s logs to stdout (no file to tail)", rs[0].DisplayName()))
		return
	}
	if err := pc.WriteStatusLine(0, "ok"); err != nil {
		return
	}
	streamFile(ctx, conn, pc, cfg.Log.Stdout, s.log)
}

// streamFile is the actual follow loop, factored out so future
// callers (e.g. tail --follow on stderr) can reuse it. Initial
// dump is the last 8KB to match one-shot tail; then poll every
// 200ms for size growth, reopening on inode change. Exits when
// ctx fires or a write to the client fails.
func streamFile(ctx context.Context, conn net.Conn, pc *ctlproto.Conn, path string, log *slog.Logger) {
	const initialTail = int64(8192)
	const pollInterval = 200 * time.Millisecond

	openWithCheck := func() (*os.File, os.FileInfo, error) {
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, nil, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		if !st.Mode().IsRegular() {
			f.Close()
			return nil, nil, fmt.Errorf("not a regular file: %s", path)
		}
		return f, st, nil
	}

	f, st, err := openWithCheck()
	if err != nil {
		_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: %v", err))
		return
	}
	defer f.Close()

	// Emit the last initialTail bytes as the snapshot, just like
	// one-shot `tail`. Pin the offset to the start of the first
	// complete line so half-line snippets don't appear mid-stream.
	offset := st.Size() - initialTail
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: seek: %v", err))
		return
	}
	reader := bufio.NewReader(f)
	if offset > 0 {
		// Drop the first (likely partial) line.
		if _, err := reader.ReadString('\n'); err != nil && err != io.EOF {
			_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: read: %v", err))
			return
		}
	}
	if err := emitAvailable(reader, pc, conn); err != nil {
		return
	}

	prevIno := inodeOf(st)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Detect rotation via inode change: logrotate renames the old
		// file out and creates a new one at the same path. When that
		// happens, reopen and reset the reader. Without this, the
		// follow loop would stay parked on the renamed (now dead)
		// inode and never see the new logs.
		newSt, statErr := os.Stat(path)
		if statErr == nil && inodeOf(newSt) != prevIno {
			f.Close()
			f, _, err = openWithCheck()
			if err != nil {
				_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: reopen: %v", err))
				return
			}
			reader = bufio.NewReader(f)
			prevIno = inodeOf(newSt)
			log.Info("tail --follow: file rotated; reopened", "path", path)
		}
		if err := emitAvailable(reader, pc, conn); err != nil {
			return
		}
	}
}

// emitAvailable drains every complete line currently in the reader,
// writes each as a body line, and returns nil at EOF (more bytes
// may arrive later). Returns an error if the client write fails so
// the streaming loop can exit promptly on disconnect.
func emitAvailable(reader *bufio.Reader, pc *ctlproto.Conn, conn net.Conn) error {
	// Refresh the write deadline on every drain so a long-running
	// follow doesn't time out on the kernel's socket buffer side.
	_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if werr := pc.WriteBodyLine(trimmed); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// Read error other than EOF: surface and stop.
			_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: read: %v", err))
			return err
		}
	}
}

// inodeOf extracts the inode from a FileInfo via the underlying
// syscall.Stat_t. Linux-specific in spirit; on macOS the same
// field exists so this works for dev as well. Returns 0 if the
// info doesn't expose the syscall struct (no platform we ship to
// today hits that).
func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
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

// cmdReady is the scheduler-friendly "is this container's stack up?"
// verb. Exits 0 (code 0 on the wire) iff every selected service is
// currently Running AND has either passed its [ready] probe or has
// no probe configured. With no args, considers every runner; with
// args, restricts to the named services/replicas (same syntax as
// other verbs). The body lists each not-ready runner with the
// reason ("starting", "backoff", "[ready] not passed", etc.) so an
// operator (or a CI pipeline running `zpctl ready` between deploys)
// can diagnose without a separate `zpctl status` call.
//
// A service that was ready and is currently in Backoff stays
// "ready" in the readyPassed sense but counts as not-ready here
// because State != Running. The intent of `ready` is "right now,
// is the stack serving"; a transient backoff dip is therefore a
// non-zero exit (which is what schedulers want to see).
func (s *ControlServer) cmdReady(args []string) *ctlproto.Response {
	targets, err := s.expandTargets(args, true)
	if err != nil {
		return errResp(err.Error())
	}
	resp := okResp("ok")
	var notReady []string
	for _, r := range targets {
		snap := r.Snapshot()
		ready := snap.State == StateRunning && (r.Cfg().Ready == nil || snap.ReadyPassed)
		if ready {
			continue
		}
		reason := readyReason(snap, r.Cfg().Ready != nil)
		notReady = append(notReady, fmt.Sprintf("%s: %s", r.DisplayName(), reason))
	}
	if len(notReady) == 0 {
		resp.Body = []string{fmt.Sprintf("all %d service(s) ready", len(targets))}
		return resp
	}
	resp.Body = notReady
	resp.Code = 1
	resp.Msg = fmt.Sprintf("%d/%d not ready", len(notReady), len(targets))
	return resp
}

// readyReason produces a short human-readable note for a not-ready
// runner. Picks the most informative of:
//
//	state != Running        → state name (BACKOFF / FATAL / STOPPED / ...)
//	state == Running, probe → "[ready] not passed"
func readyReason(snap Status, hasProbe bool) string {
	if snap.State != StateRunning {
		return strings.ToLower(string(snap.State))
	}
	if hasProbe && !snap.ReadyPassed {
		return "[ready] not passed"
	}
	return "not ready"
}

func (s *ControlServer) cmdHelp() *ctlproto.Response {
	return okBody("ok", []string{
		"status [--verbose] [NAME...] list service states (no args = all)",
		"start NAME[/N]...     start service(s); 'all' for everything",
		"stop NAME[/N]...      stop service(s); 'all' for everything",
		"restart NAME[/N]...   stop then start; 'all' for everything",
		"pid [NAME[/N]]        PID of zpinit (no arg) or service replica",
		"update                reload config and apply (= SIGHUP)",
		"reload                with no args: alias for update",
		"reload NAME[/N]...    in-place reload (reload_signal/_command or full restart)",
		"reread                dry-run config diff",
		"ready [NAME[/N]...]   exit 0 iff selected services are Running and [ready] passed",
		"tail [--follow] NAME[/N] dump file-logged stdout; --follow streams new lines",
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
	snap := r.Snapshot()
	state := mapToSupervisordState(snap.State, snap.Manual)
	return fmt.Sprintf("%-32s %-9s %s", r.DisplayName(), state, stateDetail(snap))
}

// formatStatusLineVerbose returns the verbose status row: the
// regular state line plus key=value pairs for the data operators
// typically reach for during triage but otherwise have to assemble
// from `cat /proc/$(zpctl pid svc)/status` and `zpctl status` runs.
// Pure read; no side effects, no rate-limiting (this is a human-
// driven command, not a polling target).
//
// RSS/CPU/FDs come from /proc and are only meaningful when the
// service is actually running with a PID; the formatter prints them
// only in that case. last_exit / spawns are always meaningful.
func formatStatusLineVerbose(r *Runner) string {
	snap := r.Snapshot()
	state := mapToSupervisordState(snap.State, snap.Manual)
	base := fmt.Sprintf("%-32s %-9s %s", r.DisplayName(), state, stateDetail(snap))

	var extras []string
	if snap.PID > 0 {
		ps := readProcStats(snap.PID)
		if ps.RSSBytes > 0 {
			extras = append(extras, fmt.Sprintf("rss=%s", formatBytes(ps.RSSBytes)))
		}
		if ps.CPUSeconds > 0 {
			extras = append(extras, fmt.Sprintf("cpu=%s", formatCPU(ps.CPUSeconds)))
		}
		if ps.FDCount > 0 {
			extras = append(extras, fmt.Sprintf("fds=%d", ps.FDCount))
		}
	}
	extras = append(extras, fmt.Sprintf("spawns=%d", snap.TotalSpawns))
	if le := snap.LastExit; le.PID != 0 {
		if le.Signaled {
			extras = append(extras, fmt.Sprintf("last_exit=signal:%s", le.Signal.String()))
		} else {
			extras = append(extras, fmt.Sprintf("last_exit=code:%d", le.ExitCode))
		}
	}
	return base + "  " + strings.Join(extras, " ")
}

// formatBytes renders a byte count for verbose status. Picks the
// biggest unit that yields a value >= 1 (using binary 1024-based
// units to match what /proc reports).
func formatBytes(n uint64) string {
	const (
		Ki = uint64(1) << 10
		Mi = uint64(1) << 20
		Gi = uint64(1) << 30
	)
	switch {
	case n >= Gi:
		return fmt.Sprintf("%.1fGiB", float64(n)/float64(Gi))
	case n >= Mi:
		return fmt.Sprintf("%.1fMiB", float64(n)/float64(Mi))
	case n >= Ki:
		return fmt.Sprintf("%.1fKiB", float64(n)/float64(Ki))
	}
	return fmt.Sprintf("%dB", n)
}

// formatCPU renders accumulated CPU seconds as Hh:Mm:Ss or Mm:Ss,
// matching supervisord-style readability.
func formatCPU(secs float64) string {
	totalSecs := int(secs)
	h := totalSecs / 3600
	m := (totalSecs % 3600) / 60
	s := totalSecs % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
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

// stateDetail renders the per-state suffix from a single Runner
// Snapshot, so the "RUNNING pid 0" / "RUNNING pid X, uptime 0s"
// race window that sequential per-field accessors expose can't
// happen. DisplayName isn't on Status because it's derived from
// immutable Runner fields and doesn't need the lock.
func stateDetail(snap Status) string {
	switch snap.State {
	case StateRunning:
		if snap.UpSince.IsZero() {
			return fmt.Sprintf("pid %d", snap.PID)
		}
		return fmt.Sprintf("pid %d, uptime %s", snap.PID, formatUptime(time.Since(snap.UpSince)))
	case StateBackoff:
		return fmt.Sprintf("backoff (crashes %d)", snap.Crashes)
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
