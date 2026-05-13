package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/resources"
	"github.com/0ploy/zpinit/internal/service"
)

// reloadCommandTimeout caps how long a reload_command may run before
// we give up waiting for it. Generous: most reload commands return
// in well under a second, but a misconfigured one shouldn't pin the
// dispatch goroutine indefinitely. The orphan keeps running and gets
// reaped normally; we just stop waiting on it.
const reloadCommandTimeout = 30 * time.Second

// ReloadService reloads every runner in `group` in parallel, the
// same parallelism rule that stopRunnerGroup uses for replica
// shutdowns. Returns the joined errors. Caller is expected to pass
// a group that is the result of resolveTarget (i.e. every replica
// of one service name) or a single runner; cross-service grouping
// is the orchestrator's job, not this helper's.
//
// Dispatch is per-runner:
//
//   - cfg.ReloadSignal set → send signal to the runner's process
//     group. Fast, in-place; the running process re-reads its
//     config (or whatever it's wired to do on the signal).
//   - cfg.ReloadCommand set → spawn a transient command via the
//     centralized reaper, with the runner's env. Used for apps
//     whose reload entry-point is a CLI (e.g. `nginx -s reload`).
//   - neither → full stop+start cycle, identical to `zpctl restart`.
//     Lets operators say "reload" everywhere and have it do the
//     right thing for each service.
func (o *Orchestrator) ReloadService(ctx context.Context, group []*Runner) error {
	if len(group) == 0 {
		return nil
	}
	errs := make([]error, len(group))
	var wg sync.WaitGroup
	for i, r := range group {
		wg.Add(1)
		go func(i int, r *Runner) {
			defer wg.Done()
			errs[i] = o.reloadOne(ctx, r)
		}(i, r)
	}
	wg.Wait()
	var nonNil []error
	for _, e := range errs {
		if e != nil {
			nonNil = append(nonNil, e)
		}
	}
	return errors.Join(nonNil...)
}

func (o *Orchestrator) reloadOne(ctx context.Context, r *Runner) error {
	cfg := r.Cfg()
	name := r.DisplayName()
	switch {
	case cfg.ReloadSignal != "":
		sig, ok := config.ParseSignal(cfg.ReloadSignal)
		if !ok {
			// Validation should have rejected this at load time;
			// guard anyway so a programming error here doesn't crash
			// the dispatch goroutine.
			return fmt.Errorf("%s: invalid reload_signal %q", name, cfg.ReloadSignal)
		}
		o.log.Info("reload: signal", "service", name, "signal", cfg.ReloadSignal)
		if err := r.SignalGroup(sig); err != nil {
			return fmt.Errorf("%s: signal: %w", name, err)
		}
		return nil

	case len(cfg.ReloadCommand) > 0:
		// Merge the runner's per-service env onto the orchestrator's
		// current baseEnv, exactly like a normal spawn would, so the
		// reload command sees the same view its parent service does
		// (including ZPINIT_CPU_COUNT etc.).
		o.mu.RLock()
		env := service.MergeEnv(o.baseEnv, cfg.Env)
		o.mu.RUnlock()
		o.log.Info("reload: command", "service", name, "cmd", cfg.ReloadCommand)
		exitCh, err := o.oneShot(name, cfg.ReloadCommand, env)
		if err != nil {
			return fmt.Errorf("%s: reload_command start: %w", name, err)
		}
		wctx, cancel := context.WithTimeout(ctx, reloadCommandTimeout)
		defer cancel()
		select {
		case info := <-exitCh:
			if info.Signaled {
				o.log.Warn("reload: command killed", "service", name, "signal", info.Signal.String())
				return fmt.Errorf("%s: reload_command killed by %s", name, info.Signal.String())
			}
			if info.ExitCode != 0 {
				o.log.Warn("reload: command non-zero", "service", name, "code", info.ExitCode)
				// Non-zero exit is logged but not returned as an error
				// from ReloadService: the live service is unaffected
				// and operators can read the log. Returning would
				// promote a benign reload-script warning into a
				// scary-looking zpctl error.
			}
			return nil
		case <-wctx.Done():
			return fmt.Errorf("%s: reload_command did not finish within %s", name, reloadCommandTimeout)
		}

	default:
		return o.reloadByRestart(ctx, r)
	}
}

// reloadByRestart is the fallback when neither reload_signal nor
// reload_command is configured. Mirrors cmdStartStopRestart's
// "restart" branch but with the orchestrator-aware logging that
// "reload" implies.
func (o *Orchestrator) reloadByRestart(ctx context.Context, r *Runner) error {
	name := r.DisplayName()
	o.log.Info("reload: restart", "service", name)
	if err := r.StopCtx(ctx); err != nil {
		return fmt.Errorf("%s: stop: %w", name, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, r.Cfg().StopTimeout.Std()+reapGrace)
	state, werr := r.WaitTerminal(waitCtx)
	cancel()
	if werr != nil {
		return fmt.Errorf("%s: did not stop within timeout (state=%s): %w", name, state, werr)
	}
	if err := r.StartCtx(ctx); err != nil {
		return fmt.Errorf("%s: start: %w", name, err)
	}
	return nil
}

// OnResourceChange is the orchestrator's hook for the resource
// watcher: invoked from a goroutine in main.go every time the
// watcher commits a debounced delta. Updates the live resourceEnv
// (so future spawns see the new values) and fans out per-service
// reload actions for any runner whose reload_on_change list
// intersects the dimensions that moved.
//
// Reload runs detached on the orchestrator's runner-lifetime ctx,
// not the watcher's ctx, so a shutting-down watcher doesn't yank
// the reload mid-flight. Errors are logged; we never propagate
// them back to the watcher because there's no client to send them
// to.
func (o *Orchestrator) OnResourceChange(change resources.Change) {
	// reloadMu serializes the SetResourceEnv → SetCurrentSnapshot →
	// scaleAutoServices triad against any concurrent Reload. Without
	// this, a SIGHUP or `zpctl update` racing with a watcher commit
	// could observe a half-updated o.cfg (snapshot already advanced,
	// per-service Replicas.N still being written) or overwrite the
	// scaler's Replicas.N mutation with a stale disk-loaded value.
	// Held only across the cfg-mutating triad; the reload-on-change
	// fanout below runs outside the lock because each per-runner
	// reload only touches that runner's state.
	o.reloadMu.Lock()
	newEnv := change.Snapshot.EnvVars()
	o.SetResourceEnv(newEnv)
	o.SetCurrentSnapshot(change.Snapshot)

	// Auto-replicated services rebalance to the new target before
	// reload-on-change fans out. Existing replicas that survive the
	// rebalance still get reloaded by the fanout below so they pick
	// up the new env on their next spawn; freshly-spawned replicas
	// get reloaded again as well, which is wasteful but keeps the
	// dispatch logic simple. v1 trade-off; we can teach the fanout
	// to exempt fresh replicas later.
	o.scaleAutoServices(context.Background(), change.Snapshot)
	o.reloadMu.Unlock()

	dimset := map[string]struct{}{}
	for _, d := range change.Dimensions {
		dimset[d] = struct{}{}
	}

	o.mu.RLock()
	var affected []*Runner
	for _, r := range o.runners {
		cfg := r.Cfg()
		if len(cfg.ReloadOnChange) == 0 {
			continue
		}
		for _, want := range cfg.ReloadOnChange {
			if _, ok := dimset[want]; ok {
				affected = append(affected, r)
				break
			}
		}
	}
	parent := o.runnerCtx
	o.mu.RUnlock()

	if len(affected) == 0 {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	o.log.Info("resource change: reloading subscribed services",
		"count", len(affected), "dimensions", change.Dimensions)
	go func() {
		if err := o.reloadAcrossGroups(parent, affected); err != nil {
			o.log.Warn("reload-on-change failed", "err", err)
		}
	}()
}

// reloadAcrossGroups dispatches a reload across every filename group
// represented in `runners`, parallel within group and serial between
// groups (filename order). Same shape stopAll uses, applied to
// reload semantics. Used when `zpctl reload all` runs.
func (o *Orchestrator) reloadAcrossGroups(ctx context.Context, runners []*Runner) error {
	if len(runners) == 0 {
		return nil
	}
	// Group by filename preserving filename-sorted order.
	groups := map[string][]*Runner{}
	var order []string
	for _, r := range runners {
		fn := r.Cfg().Filename
		if _, ok := groups[fn]; !ok {
			order = append(order, fn)
		}
		groups[fn] = append(groups[fn], r)
	}
	sort.Strings(order)
	var errs []error
	for _, fn := range order {
		if err := o.ReloadService(ctx, groups[fn]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
