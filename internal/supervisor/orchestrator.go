package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
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
	// baseEnvBuilder, if set, is invoked by Reload with the new
	// globals.Env to recompute baseEnv. main.go installs this so
	// SIGHUP can propagate globals.Env changes to restarted services
	// without re-running entrypoint.d. nil means baseEnv is fixed at
	// construction (the default for tests).
	baseEnvBuilder func(globalsEnv map[string]string) []string
	log            *slog.Logger

	// Dependency hooks — fields rather than constructor args so tests
	// in the same package can swap them after NewOrchestrator without
	// having to thread them through. Production wires real
	// service.Spawn / defaultProber / RealClock.
	spawner Spawner
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
}

// SetBaseEnvBuilder installs a function that Reload uses to recompute
// the per-service base env from the new globals.Env. Optional; if
// unset, Reload leaves baseEnv unchanged.
func (o *Orchestrator) SetBaseEnvBuilder(fn func(globalsEnv map[string]string) []string) {
	o.mu.Lock()
	o.baseEnvBuilder = fn
	o.mu.Unlock()
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
	runnersSnap := append([]*Runner(nil), o.runners...)
	o.mu.Unlock()

	for _, r := range runnersSnap {
		o.spawnRunnerGoroutine(r)
	}

	bootCtx, bootCancel := context.WithTimeout(ctx, o.cfg.Globals.BootTimeout.Std())
	bootErr := o.boot(bootCtx)
	bootCancel()
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
	// iteration capture does.
	o.mu.RLock()
	snap := append([]*Runner(nil), o.runners...)
	o.mu.RUnlock()
	for _, r := range snap {
		if err := o.bootOne(ctx, r); err != nil {
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
		env := service.MergeEnv(r.baseEnv, cfg.Env)
		if err := waitReady(ctx, cfg.Ready, env, cfg.Cwd, o.prober, o.log); err != nil {
			if cfg.Ready.OnTimeout == config.ReadyContinue {
				o.log.Warn("readiness failed; continuing per on_timeout", "service", name, "err", err)
				return nil
			}
			return fmt.Errorf("readiness: %w", err)
		}
		o.log.Info("boot: ready", "service", name)
	}
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
	wctx, wcancel := context.WithCancel(o.runnerCtx)
	o.watcherCancel = wcancel
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
		o.log.Info("exit_code_from terminal", "service", name, "state", state)
		once.Do(func() { close(earlyCh) })
	}()
}

// stopAll tears services down in reverse start order, fully draining
// each one before signalling the next. Sequential teardown matters
// because filename order encodes dependency order during boot — e.g.
// php-fpm boots after redis, so on shutdown php-fpm must finish
// flushing through redis before redis itself receives SIGTERM.
// Parallel waits would signal everything in the same instant and lose
// that property. Per-runner SIGKILL escalation
// (handleStopKillTimeout) keeps any one stuck service from holding up
// the others past stop_timeout + reapGrace. ShutdownBudget at signal
// time reports the conservative serial total so the outer
// runSupervise wait covers stopAll exactly.
func (o *Orchestrator) stopAll() {
	o.mu.RLock()
	snap := append([]*Runner(nil), o.runners...)
	o.mu.RUnlock()

	for i := len(snap) - 1; i >= 0; i-- {
		r := snap[i]
		switch r.State() {
		case StateStopped, StateFatal, StatePending:
			continue
		}
		cfg := r.Cfg()
		name := r.DisplayName()
		o.log.Info("stop: signaling", "service", name)
		ctx, cancel := context.WithTimeout(context.Background(), cfg.StopTimeout.Std()+reapGrace)
		if err := r.StopCtx(ctx); err != nil {
			o.log.Warn("stop signal could not be delivered", "service", name, "err", err)
			cancel()
			continue
		}
		if _, err := r.WaitTerminal(ctx); err != nil {
			// Even SIGKILL didn't bring it down within the grace —
			// process is likely stuck in uninterruptible kernel
			// sleep. Move on to the next service rather than
			// blocking the rest of the teardown forever.
			o.log.Error("service did not terminate even after SIGKILL escalation",
				"service", name, "state", r.State(), "err", err)
		}
		cancel()
	}
}

// ShutdownBudget reports the conservative wall-clock budget that
// stopAll needs against the *current* runner set. runSupervise calls
// this at signal time so reload-induced changes (added services,
// bumped stop_timeouts) are honored — the previous boot-time snapshot
// could expire mid-teardown and cause the runtime to hard-kill PID 1
// before the configured grace window finished.
func (o *Orchestrator) ShutdownBudget() time.Duration {
	o.mu.RLock()
	defer o.mu.RUnlock()
	total := shutdownHeadroom
	for _, r := range o.runners {
		total += r.Cfg().StopTimeout.Std() + reapGrace
	}
	return total
}

func (o *Orchestrator) spawnRunnerGoroutine(r *Runner) {
	runCtx, cancel := context.WithCancel(o.runnerCtx)
	r.setRunCancel(cancel)
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		r.Run(runCtx)
	}()
}

// Reload diffs the current service set against newCfg and applies
// add/remove/restart actions. Identity is by filename per the spec
// ("file rename = remove + add"); service-name reuse across renames
// is not preserved.
//
// Restart is implemented as remove + add. A `reloadable = false`
// service whose config changed is left untouched and logged.
//
// reloadMu serializes concurrent Reloads (e.g. SIGHUP and `zpctl
// update` arriving simultaneously). Per-mutation locking (o.mu) keeps
// status/findRunner readers safe during the reload.
//
// Boot semantics: removes are synchronous. Newly-added (and
// restart-new) services are registered synchronously but their boot
// (StartCtx + WaitBootResult + readiness probe) is processed
// serially in filename order by a single detached goroutine. This
// matches initial-boot ordering — a later service can't observe an
// earlier service still booting — at the cost of returning before
// reload is fully complete. Detachment is required because sum of
// per-service boot_timeouts exceeds any reasonable client deadline.
//
// Returns a non-nil error if any service failed to terminate during
// remove/restart-stop. Failed runners stay registered so their Run
// goroutine keeps tracking the still-live process; a follow-up
// reload will retry.
func (o *Orchestrator) Reload(ctx context.Context, newCfg *config.Config) error {
	o.reloadMu.Lock()
	defer o.reloadMu.Unlock()

	o.mu.RLock()
	diff := o.computeDiffLocked(newCfg)
	builder := o.baseEnvBuilder
	o.mu.RUnlock()
	// Recompute baseEnv from the new globals.Env if main.go installed
	// a builder. Without one, fall back to the existing baseEnv (tests
	// that don't care about env propagation rely on this).
	newBaseEnv := o.baseEnv
	if builder != nil {
		newBaseEnv = builder(newCfg.Globals.Env)
	}
	o.log.Info("reload", "add", len(diff.add), "remove", len(diff.remove), "restart", len(diff.restart))

	var errs []error
	for _, r := range diff.remove {
		if err := o.removeService(ctx, r); err != nil {
			o.log.Error("reload: remove failed; runner kept registered", "service", r.Cfg().Name, "err", err)
			errs = append(errs, err)
		}
	}

	// Pair restart-new specs with successful removals. For replicated
	// services every old replica must stop before any new replica is
	// expanded; a partial stop would double-register the same filename
	// and break the diff invariant. If any old replica fails to stop,
	// keep the rest registered (removeService leaves a failed runner
	// in place on purpose) and skip the new spec for this reload —
	// next reload's diff retries.
	var addSpecs []config.Service
	for _, p := range diff.restart {
		failed := false
		for _, oldR := range p.old {
			if err := o.removeService(ctx, oldR); err != nil {
				o.log.Error("reload: restart-stop failed; new spec skipped", "service", oldR.Cfg().Name, "err", err)
				errs = append(errs, err)
				failed = true
			}
		}
		if failed {
			continue
		}
		addSpecs = append(addSpecs, p.new)
	}
	addSpecs = append(addSpecs, diff.add...)
	sort.Slice(addSpecs, func(i, j int) bool {
		return addSpecs[i].Filename < addSpecs[j].Filename
	})

	// Expand every (re)added service into per-replica boot jobs. Sort
	// is preserved at the filename level by addSpecs above; replica
	// indices then run in 0..N-1 order within a filename.
	var jobs []reloadBootJob
	for _, s := range addSpecs {
		runners := expandServiceToRunners(s, newBaseEnv, o.spawner, o.clock, o.log)
		for _, r := range runners {
			jobs = append(jobs, reloadBootJob{cfg: r.Cfg(), runner: r})
		}
	}

	// Register runners + commit cfg + baseEnv + sort under one critical
	// section so external readers see a consistent set. Spawning the
	// per-runner goroutines happens after the unlock —
	// spawnRunnerGoroutine takes no orchestrator locks but does pull
	// o.runnerCtx via setRunCancel.
	o.mu.Lock()
	for _, j := range jobs {
		o.runners = append(o.runners, j.runner)
	}
	sortRunners(o.runners)
	o.cfg = newCfg
	o.baseEnv = newBaseEnv
	o.mu.Unlock()

	for _, j := range jobs {
		o.spawnRunnerGoroutine(j.runner)
	}

	if len(jobs) > 0 {
		// Detached: caller-ctx is intentionally NOT honored here.
		// SIGHUP-driven reloads come from main.go which never cancels
		// its ctx (the timeout-bound context.WithDeadline used by the
		// control server is also too tight for a multi-service boot).
		// Tying boots to o.runnerCtx ties their lifetime to the
		// supervisor itself.
		bootRoot := o.runnerCtx
		bootJobs := append([]reloadBootJob(nil), jobs...)
		globals := newCfg.Globals
		go o.runReloadBoots(bootRoot, bootJobs, globals)
	}

	// Rebind the exit_code_from watcher: the watched service may have
	// been added in this reload, or the target name may have changed.
	o.installExitCodeWatcher()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// reloadBootJob carries the pre-built runner and its config through
// Reload's serial boot phase. Built synchronously inside Reload and
// drained one at a time by runReloadBoots in filename order.
type reloadBootJob struct {
	cfg    config.Service
	runner *Runner
}

// runReloadBoots drives the post-Reload boot sequence: each job is
// Started, awaited to Running, then probed for readiness — fully
// serially, mirroring initial boot. A boot failure logs and moves on
// to the next service; the per-runner restart loop handles recovery
// for transient errors.
//
// reloadBootMu serializes this against any prior reload's still-running
// boot phase, so two back-to-back reloads do not interleave their adds
// and break filename order. Boot goroutines are not part of the Run
// waitgroup; on orchestrator shutdown they die with the process, so a
// plain mutex without ctx-aware acquisition is sufficient.
func (o *Orchestrator) runReloadBoots(root context.Context, jobs []reloadBootJob, globals config.Globals) {
	o.reloadBootMu.Lock()
	defer o.reloadBootMu.Unlock()
	for _, j := range jobs {
		if root.Err() != nil {
			return
		}
		bctx, bcancel := context.WithTimeout(root, globals.BootTimeout.Std())
		o.bootReloadJob(bctx, j)
		bcancel()
	}
}

func (o *Orchestrator) bootReloadJob(ctx context.Context, j reloadBootJob) {
	cfg := j.cfg
	r := j.runner
	name := r.DisplayName()
	o.log.Info("reload: booting", "service", name)
	if err := r.StartCtx(ctx); err != nil {
		o.log.Error("reload: added service start signal failed", "service", name, "err", err)
		return
	}
	if err := r.WaitBootResult(ctx); err != nil {
		o.log.Error("reload: added service failed to boot", "service", name, "err", err)
		return
	}
	if cfg.Ready != nil {
		env := service.MergeEnv(r.baseEnv, cfg.Env)
		if err := waitReady(ctx, cfg.Ready, env, cfg.Cwd, o.prober, o.log); err != nil {
			if cfg.Ready.OnTimeout == config.ReadyContinue {
				o.log.Warn("reload: readiness failed; continuing per on_timeout",
					"service", name, "err", err)
			} else {
				o.log.Error("reload: added service readiness failed",
					"service", name, "err", err)
			}
		}
	}
}

type reloadRestartPair struct {
	// old holds every running replica of the service. For non-replicated
	// services it has exactly one entry; for replicas > 1 it has N. All
	// must stop before the new spec is expanded into a fresh set of
	// runners.
	old []*Runner
	new config.Service
}

type reloadDiff struct {
	add []config.Service
	// remove is a flat list of runners to deregister. Multiple replicas
	// of the same filename produce multiple entries; the order is
	// (filename ASC, replicaIndex ASC) so restart-stop and remove
	// progress predictably.
	remove  []*Runner
	restart []reloadRestartPair
}

// computeDiff is the public-test entry point that takes the lock.
// computeDiffLocked is the internal version used by Reload while it
// already holds o.mu.RLock.
func (o *Orchestrator) computeDiff(newCfg *config.Config) reloadDiff {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.computeDiffLocked(newCfg)
}

// computeDiffLocked produces a stable, filename-sorted action list.
// Pure function aside from reading o.runners; caller must hold o.mu.
//
// Replicated services map a single filename to N runners, all sharing
// the same Spec(). The diff key remains the filename: a change to the
// service spec restarts every replica; an unchanged spec leaves all
// replicas alone.
func (o *Orchestrator) computeDiffLocked(newCfg *config.Config) reloadDiff {
	existing := map[string][]*Runner{}
	for _, r := range o.runners {
		fn := r.Cfg().Filename
		existing[fn] = append(existing[fn], r)
	}
	newSet := map[string]config.Service{}
	for _, s := range newCfg.Services {
		newSet[s.Filename] = s
	}

	allFiles := map[string]struct{}{}
	for fn := range existing {
		allFiles[fn] = struct{}{}
	}
	for fn := range newSet {
		allFiles[fn] = struct{}{}
	}
	ordered := make([]string, 0, len(allFiles))
	for fn := range allFiles {
		ordered = append(ordered, fn)
	}
	sort.Strings(ordered)

	var diff reloadDiff
	for _, fn := range ordered {
		oldRunners, hasOld := existing[fn]
		s, hasNew := newSet[fn]
		switch {
		case hasOld && !hasNew:
			diff.remove = append(diff.remove, oldRunners...)
		case !hasOld && hasNew:
			diff.add = append(diff.add, s)
		case hasOld && hasNew:
			// All replicas share the same Spec (filename is the key).
			oldSpec := oldRunners[0].Spec()
			if !servicesEqual(oldSpec, s) {
				if oldSpec.IsReloadable() {
					diff.restart = append(diff.restart, reloadRestartPair{old: oldRunners, new: s})
				} else {
					o.log.Info("reload: config changed but reloadable=false; ignoring",
						"service", oldSpec.Name, "file", fn)
				}
			}
		}
	}

	// globals.Env change propagation: every reloadable service that
	// isn't already going to be torn down needs a restart so its next
	// spawn picks up the new merged env. Long-running children can't
	// be retroactively given new env vars, so restart is the only
	// mechanism. Services with reloadable=false stay running with
	// stale env, matching the contract for the rest of the spec.
	if !envMapsEqual(o.cfg.Globals.Env, newCfg.Globals.Env) {
		inDiff := map[string]struct{}{}
		for _, r := range diff.remove {
			inDiff[r.Cfg().Filename] = struct{}{}
		}
		for _, p := range diff.restart {
			inDiff[p.old[0].Cfg().Filename] = struct{}{}
		}
		for fn, runners := range existing {
			if _, skip := inDiff[fn]; skip {
				continue
			}
			newSpec, ok := newSet[fn]
			if !ok {
				continue // already in remove
			}
			spec := runners[0].Spec()
			if !spec.IsReloadable() {
				o.log.Info("reload: globals.env changed but reloadable=false; service keeps old env",
					"service", spec.Name)
				continue
			}
			diff.restart = append(diff.restart, reloadRestartPair{old: runners, new: newSpec})
		}
	}
	return diff
}

// envMapsEqual reports whether two env maps are key/value identical. Nil
// and empty maps compare equal (consistent with reflect.DeepEqual on
// these types).
func envMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// sortRunners orders runners by (Filename, replicaIndex). Filename is
// the primary boot-order key; replicaIndex is the tiebreaker for
// services declared with replicas > 1 so `zpctl status` shows
// consumer/0 above consumer/1.
func sortRunners(rs []*Runner) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Cfg().Filename != b.Cfg().Filename {
			return a.Cfg().Filename < b.Cfg().Filename
		}
		return a.replicaIndex < b.replicaIndex
	})
}

// servicesEqual compares two service configs ignoring Filename, which
// is the diff key rather than a content field.
func servicesEqual(a, b config.Service) bool {
	a.Filename = ""
	b.Filename = ""
	return reflect.DeepEqual(a, b)
}

// removeService stops a runner gracefully and, on success, deregisters
// it from o.runners. On stop failure (StopCtx couldn't be delivered, or
// WaitTerminal expired even after SIGKILL escalation) the runner stays
// registered so its Run goroutine continues tracking the still-live
// process via SIGCHLD; the next reload diff will see it and retry.
// This is load-bearing: dropping a runner whose process is still up
// would leak an unmanaged child under PID 1 with no zpctl handle.
func (o *Orchestrator) removeService(ctx context.Context, r *Runner) error {
	cfg := r.Cfg()
	name := r.DisplayName()
	o.log.Info("reload: removing", "service", name)

	// Stop with a bounded ctx so we don't get stuck if the runner is
	// already wedged. The grace beyond StopTimeout matches stopAll.
	wctx, cancel := context.WithTimeout(ctx, cfg.StopTimeout.Std()+reapGrace)
	defer cancel()
	if err := r.StopCtx(wctx); err != nil {
		return fmt.Errorf("%s: stop send: %w", name, err)
	}
	if _, err := r.WaitTerminal(wctx); err != nil {
		return fmt.Errorf("%s did not terminate within stop_timeout (state=%s): %w", name, r.State(), err)
	}

	// Cancel the runner's Run goroutine so it exits and the Runner
	// becomes garbage-collectible. Without this, every removed service
	// leaks one goroutine until orchestrator shutdown. Only safe now
	// that we've confirmed the child is gone.
	r.cancelRun()

	// copy+nil+truncate, not append-splice: the latter leaves the
	// removed *Runner alive in the now-unreachable last slot of the
	// backing array, leaking it until the slice grows past cap. Under
	// reload churn that's a slow but unbounded leak.
	o.mu.Lock()
	for i, x := range o.runners {
		if x == r {
			copy(o.runners[i:], o.runners[i+1:])
			o.runners[len(o.runners)-1] = nil
			o.runners = o.runners[:len(o.runners)-1]
			break
		}
	}
	o.mu.Unlock()
	return nil
}

func (o *Orchestrator) findRunner(name string) *Runner {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.findRunnerLocked(name)
}

// findRunnerLocked: caller must hold o.mu (read or write).
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
