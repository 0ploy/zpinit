package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/resources"
	"github.com/0ploy/zpinit/internal/service"
)

// reapGrace is the slack added on top of each service's stop_timeout
// to cover SIGKILL → kernel kill → SIGCHLD → reaper dispatch. Used
// both for per-service stop budgets in stopAll and for the
// orchestrator-wide ShutdownBudget calculation, so the supervisor
// outer wait can never trip before stopAll's inner wait.
const reapGrace = 5 * time.Second

// shutdownHeadroom is the constant slop above sum-of-(stop_timeout +
// reapGrace) that ShutdownBudget reports — covers exit-code
// computation and final reap-drain on the way out.
const shutdownHeadroom = 30 * time.Second

// Orchestrator owns the multi-service lifecycle: ordered boot with
// readiness probes, optional shutdown trigger from a single watched
// service, and reverse-order teardown. Per-service state machines and
// restart logic live in Runner; this is just the cross-service
// orchestration on top.
type Orchestrator struct {
	baseEnv []string
	// baseEnvBuilder, if set, is invoked to recompute baseEnv. main.go
	// installs this so SIGHUP can propagate globals.Env changes to
	// restarted services without re-running entrypoint.d, and so the
	// watcher-driven resource updates can refresh ZPINIT_CPU_COUNT
	// and friends without rerunning scripts either. nil means
	// baseEnv is fixed at construction (the default for tests).
	baseEnvBuilder func(globalsEnv, resourceEnv map[string]string) []string
	// resourceEnv is the latest set of detected resource env vars
	// (ZPINIT_CPU_COUNT/CPU_QUOTA/MEMORY_BYTES). Updated by
	// OnResourceChange when the watcher commits a delta; passed
	// into baseEnvBuilder on every recompose.
	resourceEnv map[string]string
	// currentSnapshot mirrors the Watcher's last committed
	// Snapshot. Reload uses it to resolve `replicas = "auto"` in
	// freshly-loaded configs so the diff machinery sees the
	// scaled count rather than the disk-loaded 0.
	currentSnapshot resources.Snapshot
	log             *slog.Logger

	// Dependency hooks — fields rather than constructor args so tests
	// in the same package can swap them after NewOrchestrator without
	// having to thread them through. Production wires real
	// service.Spawn / defaultProber / RealClock.
	spawner Spawner
	oneShot OneShotSpawner
	prober  Prober
	clock   Clock

	// reloadMu serializes Reload-vs-Reload (e.g. SIGHUP racing with
	// `zpctl update`). Held for the synchronous duration of a reload
	// (slice mutations and watcher rebind); does NOT cover the async
	// boot of newly-added services.
	reloadMu sync.Mutex

	// reloadBootMu serializes the detached boot phase across
	// back-to-back reloads. Without this, reload N's runReloadBoots
	// could interleave with reload N+1's adds — losing the
	// filename-order invariant that initial boot relies on (a later
	// service must not start while an earlier one is still booting).
	// Separate from reloadMu so the diff phase of N+1 doesn't have
	// to wait for the boot phase of N (which can be many seconds).
	reloadBootMu sync.Mutex

	// mu protects runners and cfg. Reload takes a write lock around
	// each slice mutation; readers (status/findRunner/exitCode) take a
	// read lock and either iterate while held or copy out under it.
	mu      sync.RWMutex
	cfg     *config.Config
	runners []*Runner

	// runnerCtx and wg are populated by Run and used by spawnRunnerGoroutine.
	// Each Runner derives its own cancel-able sub-context so removeService
	// can terminate its goroutine without waiting for orchestrator shutdown.
	runnerCtx context.Context
	wg        *sync.WaitGroup

	// earlyShutdownCh is closed by the exit_code_from watcher when the
	// watched service reaches a terminal state. Run() selects on it
	// alongside ctx.Done. Created in Run; never reset (closing is
	// one-shot via shutdownOnce).
	earlyShutdownCh chan struct{}
	shutdownOnce    sync.Once
	// watcherCancel cancels the currently-active watcher goroutine, if
	// any. Reload calls this before installing a new watcher so a
	// retargeted exit_code_from doesn't fire shutdown for a stale
	// target. Mutated only under mu.
	watcherCancel context.CancelFunc
	// watcherGen identifies the current watcher installation. Each
	// installExitCodeWatcher invocation bumps it; spawned goroutines
	// capture the value at spawn time and re-check it under mu before
	// firing shutdown. Without this, the old watcher could observe
	// its target reach terminal state between WaitTerminal returning
	// and watcherCancel being called by a retarget, and would then
	// fire shutdown for a service the new config no longer cares about.
	watcherGen uint64
}

// SetBaseEnvBuilder installs a function that recomposes the
// per-service base env. Called by Reload (passes new globals.Env)
// and by OnResourceChange (passes new resourceEnv). Optional; if
// unset, both paths leave baseEnv unchanged.
func (o *Orchestrator) SetBaseEnvBuilder(fn func(globalsEnv, resourceEnv map[string]string) []string) {
	o.mu.Lock()
	o.baseEnvBuilder = fn
	o.mu.Unlock()
}

// SetCurrentSnapshot records the latest committed resource
// Snapshot. main.go calls this at boot with the seed snapshot and
// OnResourceChange updates it on every commit. Reload uses the
// value to resolve `replicas = "auto"` in newly-loaded configs.
func (o *Orchestrator) SetCurrentSnapshot(snap resources.Snapshot) {
	o.mu.Lock()
	o.currentSnapshot = snap
	o.mu.Unlock()
}

// SetResourceEnv records the current detected resource env vars,
// recomputes the orchestrator's baseEnv, and pushes the new slice
// into every live Runner. Each runner caches its own baseEnv at
// construction time and reads it on respawn; without the push, a
// fallback-restart triggered by a resource change would spawn the
// new child with stale env. Called once at boot from main.go and
// again from OnResourceChange.
func (o *Orchestrator) SetResourceEnv(env map[string]string) {
	o.mu.Lock()
	o.resourceEnv = env
	if o.baseEnvBuilder != nil && o.cfg != nil {
		o.baseEnv = o.baseEnvBuilder(o.cfg.Globals.Env, env)
	}
	newBaseEnv := o.baseEnv
	runners := append([]*Runner(nil), o.runners...)
	o.mu.Unlock()
	for _, r := range runners {
		r.SetBaseEnv(newBaseEnv)
	}
}

// NewOrchestrator builds an Orchestrator wired to the production
// spawner and prober (both backed by the given reaper).
func NewOrchestrator(cfg *config.Config, baseEnv []string, r *reaper.Reaper, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{
		cfg:     cfg,
		baseEnv: baseEnv,
		log:     log,
		spawner: serviceSpawner(r, log),
		oneShot: serviceOneShotSpawner(r, log),
		prober:  defaultProber(r, log),
		clock:   RealClock(),
	}
}

func serviceSpawner(r *reaper.Reaper, log *slog.Logger) Spawner {
	return func(svc config.Service, env []string) (Process, error) {
		p, err := service.Spawn(svc, env, r, log)
		if err != nil {
			return nil, err
		}
		return WrapServiceProcess(p), nil
	}
}

func serviceOneShotSpawner(r *reaper.Reaper, log *slog.Logger) OneShotSpawner {
	return func(name string, command, env []string) (<-chan reaper.ExitInfo, error) {
		_, ch, err := service.SpawnOneShot(name, command, env, r, log)
		return ch, err
	}
}

// Run drives the supervisor: ordered boot, then steady-state until
// either ctx is canceled or the exit_code_from service reaches a
// terminal state. Returns the supervisor exit code.
func (o *Orchestrator) Run(ctx context.Context) int {
	// Each Runner needs its own goroutine. They all share runnerCtx,
	// which we cancel on the way out so they exit cleanly. Defer order
	// is load-bearing: wg.Wait must run AFTER cancelRunners, so we defer
	// wg.Wait first (runs last on return) and cancelRunners second (runs
	// first on return) — otherwise wg.Wait blocks forever while runners
	// are still parked in select.
	var wg sync.WaitGroup
	defer wg.Wait()
	runnerCtx, cancelRunners := context.WithCancel(context.Background())
	defer cancelRunners()

	// All setup writes go through o.mu so external readers (control
	// server's snapshotRunners, the eventual installExitCodeWatcher)
	// get a happens-before edge before they observe the orchestrator
	// running. Without the lock the writes here race with anything
	// that reads o.runners/o.runnerCtx — even though those readers
	// take RLock, the writer must publish via Lock to pair.
	o.mu.Lock()
	o.runners = nil
	for _, svc := range o.cfg.Services {
		o.runners = append(o.runners, expandServiceToRunners(svc, o.baseEnv, o.spawner, o.clock, o.log)...)
	}
	sortRunners(o.runners)
	o.runnerCtx = runnerCtx
	o.wg = &wg
	o.earlyShutdownCh = make(chan struct{})
	// Reset the once so a re-Run on the same Orchestrator (only tests
	// today) can still fire early-shutdown. Pairing the reset with the
	// fresh earlyShutdownCh keeps the two consistent.
	o.shutdownOnce = sync.Once{}
	runnersSnap := append([]*Runner(nil), o.runners...)
	o.mu.Unlock()

	for _, r := range runnersSnap {
		o.spawnRunnerGoroutine(r)
	}

	bootErr := o.boot(ctx)
	if bootErr != nil {
		o.log.Error("boot failed", "err", bootErr)
		o.stopAll()
		return 1
	}
	o.log.Info("boot complete", "services", len(o.runners))

	// Optional: watch the configured exit_code_from service for terminal
	// state, which triggers an orderly shutdown of everything else.
	o.installExitCodeWatcher()

	select {
	case <-ctx.Done():
		o.log.Info("ctx canceled; shutting down")
	case <-o.earlyShutdownCh:
		o.log.Info("exit_code_from service ended; shutting down siblings")
	}

	o.stopAll()
	return o.exitCode()
}

func (o *Orchestrator) boot(ctx context.Context) error {
	// Boot reads o.runners; held under read lock while we iterate so
	// concurrent Reload sees a consistent slice. Per-service work
	// (Start, WaitBootResult, probe) does not hold the lock — only the
	// iteration capture does. boot_timeout is applied per-service so a
	// slow first service can't eat the entire boot budget and starve
	// later services of their probe window. This matches reload-boot's
	// per-job timeout and the contract documented in CLAUDE.md.
	o.mu.RLock()
	snap := append([]*Runner(nil), o.runners...)
	bootTimeout := o.cfg.Globals.BootTimeout.Std()
	o.mu.RUnlock()
	for _, r := range snap {
		bctx, bcancel := context.WithTimeout(ctx, bootTimeout)
		err := o.bootOne(bctx, r)
		bcancel()
		if err != nil {
			return fmt.Errorf("%s: %w", r.DisplayName(), err)
		}
	}
	return nil
}

func (o *Orchestrator) bootOne(ctx context.Context, r *Runner) error {
	cfg := r.Cfg()
	name := r.DisplayName()
	o.log.Info("boot: starting", "service", name)
	if err := r.StartCtx(ctx); err != nil {
		return fmt.Errorf("send start: %w", err)
	}

	if err := r.WaitBootResult(ctx); err != nil {
		return fmt.Errorf("waiting for running: %w", err)
	}

	if cfg.Ready != nil {
		env := service.MergeEnv(r.BaseEnv(), cfg.Env)
		if err := waitReady(ctx, cfg.Ready, env, cfg.Cwd, o.prober, o.log); err != nil {
			if cfg.Ready.OnTimeout == config.ReadyContinue {
				// Boot proceeds; from a `zpctl ready` standpoint the
				// operator opted into "best effort" so we count this
				// as ready (the alternative would block ready forever
				// on a service that explicitly tolerates probe
				// timeout).
				r.MarkReady()
				o.log.Warn("readiness failed; continuing per on_timeout", "service", name, "err", err)
				return nil
			}
			return fmt.Errorf("readiness: %w", err)
		}
		r.MarkReady()
		o.log.Info("boot: ready", "service", name)
	} else {
		// No [ready] configured: the service is considered ready as
		// soon as it reaches Running, so mark it immediately after
		// WaitBootResult so `zpctl ready` doesn't have to special-case
		// "no probe" everywhere.
		r.MarkReady()
	}
	return nil
}

// WaitUntilReady blocks until r is RUNNING and its [ready] probe has
// passed, or it reaches a terminal/FATAL state, or ctx expires. It is
// the wait half of `zpctl start --wait` / `restart --wait`: the caller
// has already issued StartCtx, and this mirrors bootOne's
// WaitBootResult → waitReady → MarkReady sequence so a manual start
// gets the same readiness semantics boot does (the [ready] probe is
// otherwise boot-time only). Reaching Running is bounded by the global
// boot_timeout; the probe is bounded by its own [ready].timeout.
// on_timeout=continue counts as ready, matching bootOne.
func (o *Orchestrator) WaitUntilReady(ctx context.Context, r *Runner) error {
	o.mu.RLock()
	bootTimeout := o.cfg.Globals.BootTimeout.Std()
	o.mu.RUnlock()

	bctx, bcancel := context.WithTimeout(ctx, bootTimeout)
	err := r.WaitBootResult(bctx)
	bcancel()
	if err != nil {
		return err
	}

	cfg := r.Cfg()
	if cfg.Ready == nil {
		r.MarkReady()
		return nil
	}
	env := service.MergeEnv(r.BaseEnv(), cfg.Env)
	if err := waitReady(ctx, cfg.Ready, env, cfg.Cwd, o.prober, o.log); err != nil {
		if cfg.Ready.OnTimeout == config.ReadyContinue {
			r.MarkReady()
			return nil
		}
		return fmt.Errorf("readiness: %w", err)
	}
	r.MarkReady()
	return nil
}

// installExitCodeWatcher (re)installs the watcher goroutine for the
// currently-configured exit_code_from service. Idempotent: any
// previously-running watcher is canceled first. Caller must NOT hold
// o.mu — this function takes the write lock to swap watcherCancel.
func (o *Orchestrator) installExitCodeWatcher() {
	o.mu.Lock()
	if o.watcherCancel != nil {
		o.watcherCancel()
		o.watcherCancel = nil
	}
	name := o.cfg.Globals.ExitCodeFrom
	if name == "default" {
		o.mu.Unlock()
		return
	}
	var target *Runner
	for _, r := range o.runners {
		if r.Cfg().Name == name {
			target = r
			break
		}
	}
	if target == nil {
		o.mu.Unlock()
		return
	}
	// runnerCtx is populated by Run before any reload can fire in
	// production, but defend against tests that call this directly so
	// a nil parent doesn't panic context.WithCancel.
	parent := o.runnerCtx
	if parent == nil {
		parent = context.Background()
	}
	wctx, wcancel := context.WithCancel(parent)
	o.watcherCancel = wcancel
	o.watcherGen++
	gen := o.watcherGen
	earlyCh := o.earlyShutdownCh
	once := &o.shutdownOnce
	o.mu.Unlock()

	go func() {
		state, err := target.WaitTerminal(wctx)
		if err != nil {
			// Watcher canceled (reload retarget or orchestrator shutdown);
			// don't fire early-shutdown.
			return
		}
		// Re-check under the lock that we're still the current
		// installation. A reload that retargets exit_code_from cancels
		// our wctx, but cancel does not synchronize with our progress:
		// if the old target reached terminal state at the same instant,
		// WaitTerminal can return nil here even though Reload has since
		// installed a new watcher for a different service. Firing
		// shutdown in that window would terminate the supervisor for
		// the wrong reason.
		o.mu.RLock()
		stillCurrent := o.watcherGen == gen
		o.mu.RUnlock()
		if !stillCurrent {
			return
		}
		o.log.Info("exit_code_from terminal", "service", name, "state", state)
		once.Do(func() { close(earlyCh) })
	}()
}

// stopAll tears services down in reverse filename order, fully
// draining each filename group before signaling the next. Filename
// order encodes dependency order during boot (e.g. php-fpm at 20_*
// boots after redis at 10_*), so reverse-serial teardown between
// groups lets dependents drain through their dependencies before the
// dependency receives SIGTERM. WITHIN a group (all replicas of the
// same filename), replicas are signaled and awaited in parallel:
// they are by definition the same logical service and have no flush
// ordering between each other, so making them sequential would
// multiply shutdown time by N for no semantic gain. Per-runner
// SIGKILL escalation (handleStopKillTimeout) bounds any stuck
// replica. ShutdownBudget reports a per-group conservative total so
// the outer runSupervise wait covers stopAll exactly.
func (o *Orchestrator) stopAll() {
	o.mu.RLock()
	snap := append([]*Runner(nil), o.runners...)
	o.mu.RUnlock()

	// snap is sorted by (filename, replicaIndex). Walk it in reverse,
	// peeling off groups of consecutive same-filename entries and
	// stopping each group in parallel.
	for i := len(snap); i > 0; {
		fn := snap[i-1].Cfg().Filename
		j := i
		for j > 0 && snap[j-1].Cfg().Filename == fn {
			j--
		}
		o.stopRunnerGroup(context.Background(), snap[j:i])
		i = j
	}
}

// stopRunnerGroup signals and awaits every runner in a single
// filename group in parallel. All replicas of a filename share the
// same stop_timeout (they share the spec), so a single timeout
// covers the group; per-runner SIGKILL escalation bounds any stuck
// replica. Skips runners already in a terminal state.
func (o *Orchestrator) stopRunnerGroup(ctx context.Context, group []*Runner) {
	if len(group) == 0 {
		return
	}
	timeout := group[0].Cfg().StopTimeout.Std() + reapGrace
	gctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, r := range group {
		switch r.State() {
		case StateStopped, StateFatal, StatePending:
			continue
		}
		wg.Add(1)
		go func(r *Runner) {
			defer wg.Done()
			name := r.DisplayName()
			o.log.Info("stop: signaling", "service", name)
			if err := r.StopCtx(gctx); err != nil {
				o.log.Warn("stop signal could not be delivered", "service", name, "err", err)
				return
			}
			if _, err := r.WaitTerminal(gctx); err != nil {
				o.log.Error("service did not terminate even after SIGKILL escalation",
					"service", name, "state", r.State(), "err", err)
			}
		}(r)
	}
	wg.Wait()
}

// ShutdownBudget reports the conservative wall-clock budget that
// stopAll needs against the *current* runner set. runSupervise calls
// this at signal time so reload-induced changes (added services,
// bumped stop_timeouts) are honored: the previous boot-time snapshot
// could expire mid-teardown and cause the runtime to hard-kill PID 1
// before the configured grace window finished.
//
// Budget is per-filename-group rather than per-runner, matching
// stopAll's parallel-within-group / serial-between-groups schedule.
// With replicas = N a service contributes one (stop_timeout +
// reapGrace) to the total, not N: the kernel sees N parallel signals
// and they finish concurrently.
func (o *Orchestrator) ShutdownBudget() time.Duration {
	o.mu.RLock()
	defer o.mu.RUnlock()
	total := shutdownHeadroom
	var currentFilename string
	for _, r := range o.runners {
		cfg := r.Cfg()
		if cfg.Filename != currentFilename {
			currentFilename = cfg.Filename
			total += cfg.StopTimeout.Std() + reapGrace
		}
	}
	return total
}

func (o *Orchestrator) spawnRunnerGoroutine(r *Runner) {
	// Snapshot the runner-lifetime ctx and waitgroup under the lock so
	// reloads racing with Run's setup observe a published value rather
	// than a torn read. Run is the sole writer and writes both fields
	// under o.mu.Lock; the matching read here gives the Go race
	// detector a clean happens-before edge.
	o.mu.RLock()
	parent := o.runnerCtx
	wg := o.wg
	o.mu.RUnlock()
	runCtx, cancel := context.WithCancel(parent)
	r.setRunCancel(cancel)
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.Run(runCtx)
	}()
}

// BootTimeout returns the configured global boot timeout. The control
// server uses it to size the dispatch budget for `start --wait` /
// `restart --wait`, which legitimately run until a service reaches
// RUNNING (bounded by this) plus its readiness probe timeout.
func (o *Orchestrator) BootTimeout() time.Duration {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.cfg.Globals.BootTimeout.Std()
}

// GlobalsEnv returns a copy-free reference to the running globals [env]
// map. Used by the control server to tell whether a scoped update left
// a globals change deferred. Read under the orchestrator lock; callers
// must not mutate the returned map.
func (o *Orchestrator) GlobalsEnv() map[string]string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.cfg.Globals.Env
}

// removeServiceGroup stops every runner in a filename group in
// parallel and, for each one that successfully terminated,
// deregisters it from o.runners under a single critical section.
// On stop failure (StopCtx couldn't be delivered, or WaitTerminal
// expired even after SIGKILL escalation) the affected runner stays
// registered so its Run goroutine keeps tracking the still-live
// child; the next reload diff will see it and retry. Dropping a
// runner whose process is still up would leak an unmanaged child
// under PID 1 with no zpctl handle.
//
// Returns a per-runner error slice (nil on success). Callers
// iterate this to log/collect failures.
//
// Parallelism within a group matters at scale: with replicas = 64
// and the default 10s stop_timeout, sequential removal would burn
// ~16 minutes per service group on stuck children. All replicas of
// one filename share the same spec (hence the same stop_timeout),
// and they have no inter-replica flush dependency, so signaling and
// awaiting them concurrently is correct.
func (o *Orchestrator) removeServiceGroup(ctx context.Context, group []*Runner) []error {
	if len(group) == 0 {
		return nil
	}
	timeout := group[0].Cfg().StopTimeout.Std() + reapGrace
	errs := make([]error, len(group))

	var wg sync.WaitGroup
	for i, r := range group {
		wg.Add(1)
		go func(i int, r *Runner) {
			defer wg.Done()
			name := r.DisplayName()
			o.log.Info("reload: removing", "service", name)
			wctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := r.StopCtx(wctx); err != nil {
				errs[i] = fmt.Errorf("%s: stop send: %w", name, err)
				return
			}
			if _, err := r.WaitTerminal(wctx); err != nil {
				errs[i] = fmt.Errorf("%s did not terminate within stop_timeout (state=%s): %w", name, r.State(), err)
				return
			}
			// Cancel the runner's Run goroutine so it exits and the
			// Runner becomes garbage-collectible. Only safe now that
			// the child is gone.
			r.cancelRun()
		}(i, r)
	}
	wg.Wait()

	// Deregister successes under one critical section. Single pass
	// over o.runners filtering out the removeSet so a remove of K
	// runners is O(N+K) rather than O(N*K); important when
	// MaxReplicas grows or many services are deleted in one reload.
	// Trailing slots are nil'd, matching the original copy+nil+
	// truncate pattern. Without that, the removed *Runner stays
	// referenced from the backing array's tail and leaks until the
	// slice grows past cap.
	removeSet := make(map[*Runner]struct{}, len(group))
	for idx, r := range group {
		if errs[idx] == nil {
			removeSet[r] = struct{}{}
		}
	}
	if len(removeSet) == 0 {
		return errs
	}
	o.mu.Lock()
	keep := o.runners[:0]
	for _, x := range o.runners {
		if _, drop := removeSet[x]; drop {
			continue
		}
		keep = append(keep, x)
	}
	for i := len(keep); i < len(o.runners); i++ {
		o.runners[i] = nil
	}
	o.runners = keep
	o.mu.Unlock()
	return errs
}

// findRunnerLocked: caller must hold o.mu (read or write). Match is
// by cfg.Name only; for replicated services this returns the first
// replica. Callers that need replica granularity should use
// snapshotRunners + resolveTarget instead. The only production
// caller is exitCode, which is guarded at config load against
// targeting a replicated service.
func (o *Orchestrator) findRunnerLocked(name string) *Runner {
	for _, r := range o.runners {
		if r.Cfg().Name == name {
			return r
		}
	}
	return nil
}

// snapshotRunners returns a snapshot of the current runners slice,
// safe to iterate without holding the orchestrator's lock. Used by
// the control server to avoid racing Reload.
func (o *Orchestrator) snapshotRunners() []*Runner {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]*Runner, len(o.runners))
	copy(out, o.runners)
	return out
}

// configDir returns the directory the most recently loaded config came
// from. Used by cmdUpdate to know where to reread the config from.
func (o *Orchestrator) configDir() string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.cfg.Dir
}

func (o *Orchestrator) exitCode() int {
	o.mu.RLock()
	name := o.cfg.Globals.ExitCodeFrom
	r := o.findRunnerLocked(name)
	o.mu.RUnlock()
	if name == "default" {
		return 0
	}
	if r == nil {
		return 0
	}
	info := r.LastExit()
	if info.Signaled {
		return 128 + int(info.Signal)
	}
	return info.ExitCode
}
