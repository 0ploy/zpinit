package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

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
	for _, r := range o.runners {
		wg.Add(1)
		go func(r *Runner) {
			defer wg.Done()
			r.Run(runnerCtx)
		}(r)
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

// stopAll signals each running service in reverse start order, waiting
// up to the per-service stop_timeout. Phase 6 adds SIGKILL escalation
// once the timeout elapses; until then a service that ignores its
// stop_signal will leak past zpinit's exit.
func (o *Orchestrator) stopAll() {
	for i := len(o.runners) - 1; i >= 0; i-- {
		r := o.runners[i]
		switch r.State() {
		case StateStopped, StateFatal, StatePending:
			continue
		}
		cfg := r.Cfg()
		o.log.Info("stop: signaling", "service", cfg.Name)
		r.Stop()

		ctx, cancel := context.WithTimeout(context.Background(), cfg.StopTimeout.Std())
		if _, err := r.WaitTerminal(ctx); err != nil {
			o.log.Warn("service did not stop within stop_timeout (Phase 6 will SIGKILL escalate)",
				"service", cfg.Name, "state", r.State(), "err", err)
		}
		cancel()
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
