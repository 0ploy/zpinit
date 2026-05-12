// Package supervisor owns the per-service state machine and (in later
// phases) the orchestration of multiple services. Phase 4 implements
// Runner: one service, autonomous loop, restart policy, capped+resetting
// backoff, retry budget. Phase 5 will add ordered boot and readiness
// probes; Phase 6 adds graceful shutdown with SIGKILL escalation.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// State names match the spec's state diagram. The mapping to
// supervisord-compatible status strings (RUNNING, STARTING, etc.) lives
// in the control-protocol package added in Phase 8.
type State string

const (
	StatePending  State = "pending"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateBackoff  State = "backoff"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFatal    State = "fatal"
)

// MaxConsecutiveCrashes is the retry budget after which a service goes
// fatal. Crashes past this value with no successful run interval (i.e.
// the service never reaches the BackoffResetAfter window) lead to fatal.
// Hardcoded for now; can be promoted to config if needed.
const MaxConsecutiveCrashes = 5

type cmdKind int

const (
	cmdStart cmdKind = iota
	cmdStop
)

type command struct {
	kind cmdKind
	done chan struct{}
}

// Runner drives one service: spawn, wait, restart, retry, fatal. Run
// holds the only goroutine that mutates state; external State() and
// PID() reads use the mutex.
type Runner struct {
	// cfg is the per-replica config the Spawn uses: log paths are
	// rewritten with the replica index for services with replicas > 1.
	// For single-replica services cfg is identical to spec.
	cfg config.Service
	// spec is the unmodified TOML-loaded service spec (one per
	// filename, shared across replicas). reload-diff comparisons use
	// spec rather than cfg because cfg's rewritten log paths would
	// otherwise make servicesEqual report a phantom change every
	// reload for replicated services. Callers that surface log paths
	// to the user (zpctl tail, log writer) want cfg; callers that
	// reason about the service identity want spec.
	spec    config.Service
	baseEnv []string
	spawn   Spawner
	clock   Clock
	log     *slog.Logger

	// replicaIndex is 0 for single-replica services and 0..N-1 for
	// services declared with replicas > 1. Used by DisplayName and by
	// the orchestrator's secondary sort key. Stored on the Runner
	// rather than re-derived from cfg because cfg.Replicas tells you
	// the total but not which copy this Runner is.
	replicaIndex int

	cmds chan command

	mu        sync.Mutex
	state     State
	process   Process
	lastPID   int
	lastExit  reaper.ExitInfo
	crashes   int
	upSince   time.Time
	nextDelay time.Duration

	// stoppedManually distinguishes Stopped reached via Stop command
	// (-> supervisord status STOPPED) vs reached via clean exit
	// (-> EXITED). Set true on Stop, cleared on Start.
	stoppedManually bool

	// observers, if any, receive every state transition. Buffered; sends
	// are non-blocking — observers that fall behind miss events.
	observersMu sync.Mutex
	observers   []chan State

	// runCancel cancels the Run goroutine's context. Set by the
	// orchestrator's spawnRunnerGoroutine before Run starts; called by
	// removeService so the goroutine exits and the Runner can be GC'd.
	// Nil for runners constructed outside the orchestrator (e.g. tests
	// that call NewRunner directly). Always accessed via setRunCancel/
	// cancelRun so the read in removeService gets a happens-before edge
	// to the write in spawnRunnerGoroutine.
	runCancel context.CancelFunc
}

// setRunCancel stores the cancel function for the runner's own Run
// goroutine. Lock-protected so external readers (cancelRun) observe
// the write through r.mu.
func (r *Runner) setRunCancel(cancel context.CancelFunc) {
	r.mu.Lock()
	r.runCancel = cancel
	r.mu.Unlock()
}

// cancelRun cancels the runner's Run goroutine if one was registered.
// Idempotent.
func (r *Runner) cancelRun() {
	r.mu.Lock()
	c := r.runCancel
	r.mu.Unlock()
	if c != nil {
		c()
	}
}

// NewRunner constructs a Runner in state Pending. Caller must invoke
// Run in a goroutine and then drive the lifecycle via Start/Stop.
// replicaIndex is 0 for single-copy services; for replicas > 1 the
// orchestrator passes 0..N-1 from expandServiceToRunners.
func NewRunner(cfg config.Service, baseEnv []string, replicaIndex int, spawn Spawner, clock Clock, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	if clock == nil {
		clock = RealClock()
	}
	return &Runner{
		cfg:          cfg,
		spec:         cfg,
		baseEnv:      baseEnv,
		replicaIndex: replicaIndex,
		spawn:        spawn,
		clock:        clock,
		log:          log,
		cmds:         make(chan command, 4),
		state:        StatePending,
	}
}

// Spec returns the unmodified service spec (the same content across
// all replicas of a service). Used by the orchestrator's reload diff;
// callers that need per-replica state (log paths) should use Cfg.
func (r *Runner) Spec() config.Service { return r.spec }

// ReplicaIndex returns this runner's 0-based replica index. 0 for
// single-replica services.
func (r *Runner) ReplicaIndex() int { return r.replicaIndex }

// DisplayName returns the human-facing identifier: cfg.Name for
// single-replica services, "<name>/<index>" for replicas > 1. Used
// everywhere log lines and zpctl rows surface a runner identity.
// TOML-level concepts like exit_code_from continue to use cfg.Name
// (those reference the service spec, not a specific replica).
func (r *Runner) DisplayName() string {
	if r.cfg.Replicas <= 1 {
		return r.cfg.Name
	}
	return r.cfg.Name + "/" + strconv.Itoa(r.replicaIndex)
}

// runnerTimers holds the two timer slots Run multiplexes on: backoff
// (between exit and next spawn) and stopKill (between stop_signal and
// SIGKILL escalation). At most one of each is set at any moment.
type runnerTimers struct {
	backoff  Timer
	stopKill Timer
}

func (t *runnerTimers) cancelBackoff() {
	if t.backoff != nil {
		t.backoff.Stop()
		t.backoff = nil
	}
}

func (t *runnerTimers) cancelStopKill() {
	if t.stopKill != nil {
		t.stopKill.Stop()
		t.stopKill = nil
	}
}

// Run drives the state machine until ctx is canceled. Returns when ctx
// is done — terminal Stopped/Fatal states still keep Run alive so a
// future Start (e.g. via zpctl) can revive the service.
func (r *Runner) Run(ctx context.Context) {
	timers := &runnerTimers{}
	defer func() {
		timers.cancelBackoff()
		timers.cancelStopKill()
	}()

	for {
		var exitCh <-chan reaper.ExitInfo
		if p := r.currentProcess(); p != nil {
			exitCh = p.Exit()
		}
		var backoffCh, stopKillCh <-chan time.Time
		if timers.backoff != nil {
			backoffCh = timers.backoff.Chan()
		}
		if timers.stopKill != nil {
			stopKillCh = timers.stopKill.Chan()
		}

		select {
		case <-ctx.Done():
			return

		case cmd := <-r.cmds:
			switch cmd.kind {
			case cmdStart:
				r.handleStart(timers)
			case cmdStop:
				r.handleStop(timers)
			}
			if cmd.done != nil {
				close(cmd.done)
			}

		case info := <-exitCh:
			// The process is gone; the kill timer has done its job
			// (or never fired) and is no longer needed.
			timers.cancelStopKill()
			r.handleExit(info, timers)

		case <-backoffCh:
			timers.backoff = nil
			r.handleBackoffExpired(timers)

		case <-stopKillCh:
			timers.stopKill = nil
			r.handleStopKillTimeout()
		}
	}
}

// Start brings the runner from Pending/Stopped/Fatal/Backoff to a fresh
// spawn cycle. Blocks until the command is processed by Run; if the
// Run goroutine has already exited (e.g. orchestrator shutdown) this
// will block until the cmds-channel buffer is exhausted, then forever.
// Production code should prefer StartCtx so a dead Run goroutine
// doesn't permanently hang the caller.
func (r *Runner) Start() { _ = r.sendCtx(context.Background(), cmdStart) }

// Stop signals the running process (or skips signaling if not running)
// and transitions the runner to Stopping/Stopped. Same blocking caveat
// as Start; prefer StopCtx in production.
func (r *Runner) Stop() { _ = r.sendCtx(context.Background(), cmdStop) }

// StartCtx is the context-aware variant of Start. Returns ctx.Err if
// ctx fires before Run consumes the command — which happens cleanly
// when Run has exited because its own context was canceled.
func (r *Runner) StartCtx(ctx context.Context) error { return r.sendCtx(ctx, cmdStart) }

// StopCtx is the context-aware variant of Stop.
func (r *Runner) StopCtx(ctx context.Context) error { return r.sendCtx(ctx, cmdStop) }

func (r *Runner) sendCtx(ctx context.Context, kind cmdKind) error {
	done := make(chan struct{})
	select {
	case r.cmds <- command{kind: kind, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// State returns the current state.
func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// PID returns the live child PID, or 0 if no process is running.
func (r *Runner) PID() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.process != nil {
		return r.process.PID()
	}
	return 0
}

// Crashes returns the consecutive-crash counter (test-visible).
func (r *Runner) Crashes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.crashes
}

// Observe returns a channel that receives every state transition, and
// a cleanup function the caller must invoke when done — otherwise every
// transition broadcasts forever to a dead listener and accumulates
// memory. Drops events if the buffer fills.
func (r *Runner) Observe() (<-chan State, func()) {
	ch := make(chan State, 32)
	r.observersMu.Lock()
	r.observers = append(r.observers, ch)
	r.observersMu.Unlock()
	cancel := func() {
		r.observersMu.Lock()
		for i, c := range r.observers {
			if c == ch {
				// copy+nil+truncate, not append-splice — see the same
				// pattern in orchestrator.removeService for why the
				// trailing slot must be cleared to avoid leaking.
				copy(r.observers[i:], r.observers[i+1:])
				r.observers[len(r.observers)-1] = nil
				r.observers = r.observers[:len(r.observers)-1]
				break
			}
		}
		r.observersMu.Unlock()
	}
	return ch, cancel
}

// WaitBootResult blocks until the runner reaches Running (success),
// Fatal (failure), or Stopped/Stopping (removed before boot), or ctx
// expires. Returns nil on Running, an error otherwise.
//
// Stopped/Stopping early-return matters for reload churn: addService
// spawns a boot goroutine parked here; if a follow-up Reload removes
// the service before it reaches Running, that goroutine would
// otherwise stay blocked until boot_timeout (default 60s), leaking
// the runner reference for that whole window.
//
// Subscribe-before-check ordering avoids the classic race where the
// runner transitions to the target state between a State() probe and
// the subsequent Observe call, leaving the waiter blocked forever.
func (r *Runner) WaitBootResult(ctx context.Context) error {
	ch, cancel := r.Observe()
	defer cancel()
	switch r.State() {
	case StateRunning:
		return nil
	case StateFatal:
		return errors.New("service is fatal")
	case StateStopped, StateStopping:
		return errors.New("service stopped before boot completed")
	}
	for {
		select {
		case s := <-ch:
			switch s {
			case StateRunning:
				return nil
			case StateFatal:
				return errors.New("service is fatal")
			case StateStopped, StateStopping:
				return errors.New("service stopped before boot completed")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WaitTerminal blocks until the runner reaches Stopped or Fatal, or
// ctx expires. Used by the orchestrator's stopAll to bound how long it
// waits for each service to wind down (Phase 6 will SIGKILL escalate
// once the per-service stop_timeout elapses). Subscribe-before-check
// for the same reason as WaitBootResult.
func (r *Runner) WaitTerminal(ctx context.Context) (State, error) {
	ch, cancel := r.Observe()
	defer cancel()
	if s := r.State(); s == StateStopped || s == StateFatal {
		return s, nil
	}
	for {
		select {
		case s := <-ch:
			if s == StateStopped || s == StateFatal {
				return s, nil
			}
		case <-ctx.Done():
			return r.State(), ctx.Err()
		}
	}
}

// LastExit returns the last reaped ExitInfo (zero value if the runner
// has never seen an exit). Used by the orchestrator to compute the
// supervisor exit code via exit_code_from.
func (r *Runner) LastExit() reaper.ExitInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastExit
}

// UpSince returns when the runner last entered Running, or the zero
// time if it isn't currently running. Drives "uptime" in zpctl status.
func (r *Runner) UpSince() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upSince
}

// StoppedManually reports whether the most recent terminal state was
// reached via Stop rather than a clean exit. Used to render the
// supervisord-compatible STOPPED-vs-EXITED distinction.
func (r *Runner) StoppedManually() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stoppedManually
}

// SignalGroup forwards an arbitrary signal to the running process'
// process group. Returns an error if the runner has no live process.
// Used by the control server's `signal NAME SIG` command.
func (r *Runner) SignalGroup(sig syscall.Signal) error {
	r.mu.Lock()
	p := r.process
	r.mu.Unlock()
	if p == nil {
		return errors.New("not running")
	}
	return p.SignalGroup(sig)
}

// Cfg returns the service config (read-only access for orchestration).
func (r *Runner) Cfg() config.Service {
	return r.cfg
}

func (r *Runner) currentProcess() Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.process
}

func (r *Runner) setState(s State) {
	r.mu.Lock()
	prev := r.state
	r.state = s
	r.mu.Unlock()
	if s == prev {
		return
	}
	r.log.Info("state", "service", r.DisplayName(), "from", prev, "to", s)
	r.observersMu.Lock()
	for _, ch := range r.observers {
		select {
		case ch <- s:
		default:
		}
	}
	r.observersMu.Unlock()
}

func (r *Runner) setProcess(p Process) {
	r.mu.Lock()
	if p != nil {
		r.lastPID = p.PID()
	}
	r.process = p
	r.mu.Unlock()
}

func (r *Runner) handleStart(timers *runnerTimers) {
	r.mu.Lock()
	r.stoppedManually = false
	r.mu.Unlock()
	switch r.State() {
	case StatePending, StateStopped, StateFatal:
		r.mu.Lock()
		r.crashes = 0
		r.nextDelay = r.cfg.BackoffInitial.Std()
		r.mu.Unlock()
		r.spawnNext(timers)
	case StateBackoff:
		// Caller wants the service NOW; cancel the pending backoff.
		timers.cancelBackoff()
		r.spawnNext(timers)
	default:
		// Already starting/running/stopping — Start is a no-op.
	}
}

// spawnNext spawns the configured command and transitions Starting->Running
// (the readiness probe lives in the orchestrator, not here, since it's
// cross-service ordering rather than per-service state).
// On spawn failure, increments crashes and schedules backoff (or fatal).
func (r *Runner) spawnNext(timers *runnerTimers) {
	r.setState(StateStarting)
	proc, err := r.spawn(r.cfg, r.baseEnv)
	if err != nil {
		r.log.Error("spawn failed", "service", r.DisplayName(), "err", err)
		r.recordFailure(timers)
		return
	}
	r.setProcess(proc)
	r.mu.Lock()
	r.upSince = r.clock.Now()
	r.mu.Unlock()
	r.setState(StateRunning)
}

func (r *Runner) handleExit(info reaper.ExitInfo, timers *runnerTimers) {
	r.mu.Lock()
	r.lastExit = info
	r.mu.Unlock()
	r.setProcess(nil)

	if r.State() == StateStopping {
		r.setState(StateStopped)
		return
	}

	crashed := info.Signaled || info.ExitCode != 0

	var shouldRestart bool
	switch r.cfg.Restart {
	case config.RestartAlways:
		shouldRestart = true
	case config.RestartOnFailure:
		shouldRestart = crashed
	case config.RestartNever:
		shouldRestart = false
	}

	if !shouldRestart {
		r.setState(StateStopped)
		return
	}

	// Reset backoff if the service was up long enough — without this, a
	// daemon that crashes once a day eventually has 30s restart delays.
	r.mu.Lock()
	upFor := time.Duration(0)
	if !r.upSince.IsZero() {
		upFor = r.clock.Now().Sub(r.upSince)
	}
	r.upSince = time.Time{}
	if upFor >= r.cfg.BackoffResetAfter.Std() {
		r.crashes = 0
		r.nextDelay = r.cfg.BackoffInitial.Std()
	}
	r.mu.Unlock()

	r.recordFailure(timers)
}

// recordFailure increments the crash counter and either schedules
// backoff or transitions to fatal once the retry budget is exhausted.
// Shared between spawn-failed and process-exit-needs-restart paths.
func (r *Runner) recordFailure(timers *runnerTimers) {
	r.mu.Lock()
	r.crashes++
	crashes := r.crashes
	r.mu.Unlock()
	if crashes >= MaxConsecutiveCrashes {
		r.log.Warn("retry budget exceeded; fatal", "service", r.DisplayName(), "crashes", crashes)
		r.setState(StateFatal)
		return
	}
	delay := r.backoffStep()
	r.log.Info("backoff", "service", r.DisplayName(), "delay", delay, "crashes", crashes)
	timers.backoff = r.clock.NewTimer(delay)
	r.setState(StateBackoff)
}

// backoffStep returns the current delay and advances the next one (cap
// at BackoffMax). Initialises from BackoffInitial on first call after
// reset.
func (r *Runner) backoffStep() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nextDelay <= 0 {
		r.nextDelay = r.cfg.BackoffInitial.Std()
	}
	delay := r.nextDelay
	next := delay * 2
	if max := r.cfg.BackoffMax.Std(); next > max {
		next = max
	}
	r.nextDelay = next
	return delay
}

func (r *Runner) handleBackoffExpired(timers *runnerTimers) {
	if r.State() != StateBackoff {
		return
	}
	r.spawnNext(timers)
}

func (r *Runner) handleStop(timers *runnerTimers) {
	r.mu.Lock()
	r.stoppedManually = true
	r.mu.Unlock()
	switch r.State() {
	case StateStarting, StateRunning:
		sig, ok := config.ParseSignal(r.cfg.StopSignal)
		if !ok {
			sig = syscall.SIGTERM
		}
		if p := r.currentProcess(); p != nil {
			if err := p.SignalGroup(sig); err != nil {
				r.log.Warn("SignalGroup failed", "service", r.DisplayName(), "err", err)
			}
		}
		r.setState(StateStopping)
		// Schedule SIGKILL escalation if the process doesn't exit by
		// stop_timeout. handleExit cancels the timer if the process
		// dies on its own first.
		timers.stopKill = r.clock.NewTimer(r.cfg.StopTimeout.Std())

	case StateBackoff:
		timers.cancelBackoff()
		r.setState(StateStopped)

	case StatePending:
		r.setState(StateStopped)

	default:
		// stopping/stopped/fatal — no-op (SIGKILL timer, if any, keeps running)
	}
}

// handleStopKillTimeout fires when stop_timeout has elapsed since Stop
// and the process is still alive. We escalate to SIGKILL on the process
// group; the kernel will reap and the resulting Exit will transition us
// to Stopped. Stays in Stopping until that happens — SIGKILL is
// uncatchable, so for any process not stuck in uninterruptible kernel
// sleep this is a matter of milliseconds.
func (r *Runner) handleStopKillTimeout() {
	if r.State() != StateStopping {
		return
	}
	p := r.currentProcess()
	if p == nil {
		return
	}
	r.log.Warn("stop_timeout exceeded; escalating to SIGKILL",
		"service", r.DisplayName(), "pid", p.PID(), "stop_timeout", r.cfg.StopTimeout.Std())
	if err := p.SignalGroup(syscall.SIGKILL); err != nil {
		r.log.Error("SIGKILL failed", "service", r.DisplayName(), "err", err)
	}
}
