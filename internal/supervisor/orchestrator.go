package supervisor

import (
	"context"
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

// Orchestrator owns the multi-service lifecycle: ordered boot with
// readiness probes, optional shutdown trigger from a single watched
// service, and reverse-order teardown. Per-service state machines and
// restart logic live in Runner; this is just the cross-service
// orchestration on top.
type Orchestrator struct {
	cfg     *config.Config
	baseEnv []string
	log     *slog.Logger

	// Dependency hooks — fields rather than constructor args so tests
	// in the same package can swap them after NewOrchestrator without
	// having to thread them through. Production wires real
	// service.Spawn / defaultProber / RealClock.
	spawner Spawner
	prober  Prober
	clock   Clock

	runners []*Runner

	// runnerCtx and wg are populated by Run and used by Reload to spawn
	// goroutines for newly-added services on the same lifecycle as the
	// initial set. Reload is called from the main goroutine while
	// orch.Run is parked in its steady-state select, so they don't race
	// over runners/cfg in practice — see the Reload comment for details.
	runnerCtx context.Context
	wg        *sync.WaitGroup
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
	o.runners = make([]*Runner, len(o.cfg.Services))
	for i, svc := range o.cfg.Services {
		o.runners[i] = NewRunner(svc, o.baseEnv, o.spawner, o.clock, o.log)
	}

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
	o.runnerCtx = runnerCtx
	o.wg = &wg
	for _, r := range o.runners {
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
	earlyShutdown := o.startExitCodeWatcher(ctx)

	select {
	case <-ctx.Done():
		o.log.Info("ctx canceled; shutting down")
	case <-earlyShutdown:
		o.log.Info("exit_code_from service ended; shutting down siblings")
	}

	o.stopAll()
	return o.exitCode()
}

func (o *Orchestrator) boot(ctx context.Context) error {
	for _, r := range o.runners {
		if err := o.bootOne(ctx, r); err != nil {
			return fmt.Errorf("%s: %w", r.Cfg().Name, err)
		}
	}
	return nil
}

func (o *Orchestrator) bootOne(ctx context.Context, r *Runner) error {
	cfg := r.Cfg()
	o.log.Info("boot: starting", "service", cfg.Name)
	r.Start()

	if err := r.WaitBootResult(ctx); err != nil {
		return fmt.Errorf("waiting for running: %w", err)
	}

	if cfg.Ready != nil {
		env := service.MergeEnv(o.baseEnv, cfg.Env)
		if err := waitReady(ctx, cfg.Ready, env, cfg.Cwd, o.prober, o.log); err != nil {
			if cfg.Ready.OnTimeout == config.ReadyContinue {
				o.log.Warn("readiness failed; continuing per on_timeout", "service", cfg.Name, "err", err)
				return nil
			}
			return fmt.Errorf("readiness: %w", err)
		}
		o.log.Info("boot: ready", "service", cfg.Name)
	}
	return nil
}

// startExitCodeWatcher returns a channel that closes when the
// configured exit_code_from service reaches a terminal state, or
// immediately a closed never-firing channel if exit_code_from is
// "default". The select in Run ORs this against ctx.Done.
func (o *Orchestrator) startExitCodeWatcher(ctx context.Context) <-chan struct{} {
	name := o.cfg.Globals.ExitCodeFrom
	if name == "default" {
		return nil // nil channel never fires in select
	}
	r := o.findRunner(name)
	if r == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		state, _ := r.WaitTerminal(ctx)
		o.log.Info("exit_code_from terminal", "service", name, "state", state)
	}()
	return done
}

// stopAll signals each running service in reverse start order. The
// per-runner SIGKILL escalation (handleStopKillTimeout) handles
// processes that ignore their stop_signal, so this loop just waits
// for each runner to reach Stopped/Fatal. The wait budget is
// stop_timeout plus a few seconds of slack for SIGKILL → kernel
// kill → SIGCHLD → reaper dispatch.
func (o *Orchestrator) stopAll() {
	const reapGrace = 5 * time.Second
	for i := len(o.runners) - 1; i >= 0; i-- {
		r := o.runners[i]
		switch r.State() {
		case StateStopped, StateFatal, StatePending:
			continue
		}
		cfg := r.Cfg()
		o.log.Info("stop: signaling", "service", cfg.Name)
		r.Stop()

		ctx, cancel := context.WithTimeout(context.Background(), cfg.StopTimeout.Std()+reapGrace)
		if _, err := r.WaitTerminal(ctx); err != nil {
			// Even SIGKILL didn't bring it down within the grace —
			// process is likely stuck in uninterruptible kernel
			// sleep. Nothing more we can do here.
			o.log.Error("service did not terminate even after SIGKILL escalation",
				"service", cfg.Name, "state", r.State(), "err", err)
		}
		cancel()
	}
}

func (o *Orchestrator) spawnRunnerGoroutine(r *Runner) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		r.Run(o.runnerCtx)
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
// Safe to call concurrently with orch.Run because orch.Run only reads
// o.runners during initial boot or stopAll; both happen outside the
// steady-state window when SIGHUP is processed. exit_code_from
// pointing at a reload-removed service will fire shutdown via the
// existing watcher — that's a known interaction worth flagging in
// release notes if it bites someone.
func (o *Orchestrator) Reload(ctx context.Context, newCfg *config.Config) error {
	diff := o.computeDiff(newCfg.Services)
	o.log.Info("reload", "add", len(diff.add), "remove", len(diff.remove), "restart", len(diff.restart))

	for _, r := range diff.remove {
		o.removeService(ctx, r)
	}
	for _, p := range diff.restart {
		o.removeService(ctx, p.old)
		o.addService(ctx, p.new, newCfg.Globals)
	}
	for _, s := range diff.add {
		o.addService(ctx, s, newCfg.Globals)
	}

	// Keep runners filename-sorted so stopAll's reverse iteration matches
	// the original boot order even after adds.
	sort.Slice(o.runners, func(i, j int) bool {
		return o.runners[i].Cfg().Filename < o.runners[j].Cfg().Filename
	})

	o.cfg = newCfg
	return nil
}

type reloadRestartPair struct {
	old *Runner
	new config.Service
}

type reloadDiff struct {
	add     []config.Service
	remove  []*Runner
	restart []reloadRestartPair
}

// computeDiff produces a stable, filename-sorted action list. Pure
// function (no I/O, no goroutines), kept private but exposed via tests
// in the same package.
func (o *Orchestrator) computeDiff(newSvcs []config.Service) reloadDiff {
	existing := map[string]*Runner{}
	for _, r := range o.runners {
		existing[r.Cfg().Filename] = r
	}
	newSet := map[string]config.Service{}
	for _, s := range newSvcs {
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
		old, hasOld := existing[fn]
		s, hasNew := newSet[fn]
		switch {
		case hasOld && !hasNew:
			diff.remove = append(diff.remove, old)
		case !hasOld && hasNew:
			diff.add = append(diff.add, s)
		case hasOld && hasNew:
			if !servicesEqual(old.Cfg(), s) {
				if old.Cfg().IsReloadable() {
					diff.restart = append(diff.restart, reloadRestartPair{old: old, new: s})
				} else {
					o.log.Info("reload: config changed but reloadable=false; ignoring",
						"service", old.Cfg().Name, "file", fn)
				}
			}
		}
	}
	return diff
}

// servicesEqual compares two service configs ignoring Filename, which
// is the diff key rather than a content field.
func servicesEqual(a, b config.Service) bool {
	a.Filename = ""
	b.Filename = ""
	return reflect.DeepEqual(a, b)
}

func (o *Orchestrator) removeService(ctx context.Context, r *Runner) {
	cfg := r.Cfg()
	o.log.Info("reload: removing", "service", cfg.Name)
	r.Stop()
	wctx, cancel := context.WithTimeout(ctx, cfg.StopTimeout.Std()+5*time.Second)
	if _, err := r.WaitTerminal(wctx); err != nil {
		o.log.Warn("reload: removed service did not terminate cleanly",
			"service", cfg.Name, "err", err)
	}
	cancel()
	for i, x := range o.runners {
		if x == r {
			o.runners = append(o.runners[:i], o.runners[i+1:]...)
			break
		}
	}
	// Note: the runner's Run goroutine stays parked on runnerCtx until
	// orchestrator shutdown. Acceptable for typical reload patterns.
}

func (o *Orchestrator) addService(ctx context.Context, cfg config.Service, globals config.Globals) {
	o.log.Info("reload: adding", "service", cfg.Name)
	r := NewRunner(cfg, o.baseEnv, o.spawner, o.clock, o.log)
	o.runners = append(o.runners, r)
	o.spawnRunnerGoroutine(r)

	bctx, bcancel := context.WithTimeout(ctx, globals.BootTimeout.Std())
	defer bcancel()

	r.Start()
	if err := r.WaitBootResult(bctx); err != nil {
		o.log.Error("reload: added service failed to boot", "service", cfg.Name, "err", err)
		return
	}
	if cfg.Ready != nil {
		env := service.MergeEnv(o.baseEnv, cfg.Env)
		if err := waitReady(bctx, cfg.Ready, env, cfg.Cwd, o.prober, o.log); err != nil {
			if cfg.Ready.OnTimeout == config.ReadyContinue {
				o.log.Warn("reload: readiness failed; continuing per on_timeout",
					"service", cfg.Name, "err", err)
			} else {
				o.log.Error("reload: added service readiness failed",
					"service", cfg.Name, "err", err)
			}
		}
	}
}

func (o *Orchestrator) findRunner(name string) *Runner {
	for _, r := range o.runners {
		if r.Cfg().Name == name {
			return r
		}
	}
	return nil
}

func (o *Orchestrator) exitCode() int {
	name := o.cfg.Globals.ExitCodeFrom
	if name == "default" {
		return 0
	}
	r := o.findRunner(name)
	if r == nil {
		return 0
	}
	info := r.LastExit()
	if info.Signaled {
		return 128 + int(info.Signal)
	}
	return info.ExitCode
}
