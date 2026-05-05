package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
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
func (s *ControlServer) Listen(ctx context.Context, path string) error {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
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
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *ControlServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	pc := ctlproto.NewConn(conn)
	req, err := pc.ReadRequest()
	if err != nil {
		_ = pc.WriteResponse(&ctlproto.Response{Code: 1, Msg: "bad request: " + err.Error()})
		return
	}
	resp := s.dispatch(req)
	_ = pc.WriteResponse(resp)
}

func (s *ControlServer) dispatch(req *ctlproto.Request) *ctlproto.Response {
	switch req.Verb {
	case "status":
		return s.cmdStatus(req.Args)
	case "start":
		return s.cmdStartStopRestart(req.Args, "start")
	case "stop":
		return s.cmdStartStopRestart(req.Args, "stop")
	case "restart":
		return s.cmdStartStopRestart(req.Args, "restart")
	case "pid":
		return s.cmdPID(req.Args)
	case "update", "reload":
		return s.cmdUpdate()
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

func (s *ControlServer) cmdStartStopRestart(args []string, action string) *ctlproto.Response {
	if len(args) == 0 {
		return errResp(fmt.Sprintf("usage: %s NAME [NAME...] | %s all", action, action))
	}
	targets, err := s.expandTargets(args, false)
	if err != nil {
		return errResp(err.Error())
	}
	resp := okResp("ok")
	for _, r := range targets {
		switch action {
		case "start":
			r.Start()
		case "stop":
			r.Stop()
		case "restart":
			r.Stop()
			ctx, cancel := context.WithTimeout(context.Background(),
				r.Cfg().StopTimeout.Std()+5*time.Second)
			_, _ = r.WaitTerminal(ctx)
			cancel()
			r.Start()
		}
		resp.Body = append(resp.Body, fmt.Sprintf("%s: %s", r.Cfg().Name, action))
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

func (s *ControlServer) cmdUpdate() *ctlproto.Response {
	newCfg, err := config.Load(s.orch.cfg.Dir)
	if err != nil {
		return errResp("load: " + err.Error())
	}
	if err := s.orch.Reload(context.Background(), newCfg); err != nil {
		return errResp("reload: " + err.Error())
	}
	return okResp("ok")
}

func (s *ControlServer) cmdReread() *ctlproto.Response {
	newCfg, err := config.Load(s.orch.cfg.Dir)
	if err != nil {
		return errResp("load: " + err.Error())
	}
	diff := s.orch.computeDiff(newCfg.Services)
	resp := okResp("ok")
	for _, r := range diff.remove {
		resp.Body = append(resp.Body, fmt.Sprintf("- %s (will stop)", r.Cfg().Name))
	}
	for _, p := range diff.restart {
		resp.Body = append(resp.Body, fmt.Sprintf("~ %s (will restart)", p.new.Name))
	}
	for _, svc := range diff.add {
		resp.Body = append(resp.Body, fmt.Sprintf("+ %s (will start)", svc.Name))
	}
	if len(resp.Body) == 0 {
		resp.Body = []string{"no changes"}
	}
	return resp
}

func (s *ControlServer) cmdTail(args []string) *ctlproto.Response {
	follow := false
	name := ""
	for _, a := range args {
		if a == "-f" {
			follow = true
			continue
		}
		name = a
	}
	if name == "" {
		return errResp("usage: tail [-f] NAME")
	}
	r := s.orch.findRunner(name)
	if r == nil {
		return errResp("unknown service: " + name)
	}
	cfg := r.Cfg()
	if cfg.Log.Stdout == "" || cfg.Log.Stdout == "inherit" {
		return errResp(fmt.Sprintf("%s logs to stdout (no file to tail)", name))
	}
	if follow {
		return errResp("tail -f is not yet implemented; rerun without -f for a snapshot")
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
		"tail [-f] NAME      dump last 8KB of file-logged stdout (-f not yet supported)",
		"signal NAME SIG     send arbitrary signal to service's process group",
		"shutdown            stop supervisor and exit",
		"help                this list",
	})
}

func (s *ControlServer) expandTargets(args []string, allOnEmpty bool) ([]*Runner, error) {
	if len(args) == 0 {
		if allOnEmpty {
			return s.orch.runners, nil
		}
		return nil, fmt.Errorf("no service named")
	}
	if len(args) == 1 && args[0] == "all" {
		return s.orch.runners, nil
	}
	out := make([]*Runner, 0, len(args))
	for _, n := range args {
		r := s.orch.findRunner(n)
		if r == nil {
			return nil, fmt.Errorf("unknown service: %s", n)
		}
		out = append(out, r)
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
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	offset := st.Size() - n
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return "", err
	}
	buf := make([]byte, st.Size()-offset)
	_, err = f.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func okResp(msg string) *ctlproto.Response { return &ctlproto.Response{Code: 0, Msg: msg} }

func okBody(msg string, body []string) *ctlproto.Response {
	return &ctlproto.Response{Code: 0, Msg: msg, Body: body}
}

func errResp(msg string) *ctlproto.Response { return &ctlproto.Response{Code: 1, Msg: msg} }
