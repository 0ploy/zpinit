package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
// shutdowns. Returns a per-runner error slice in input order (nil
// entry on success). Caller is expected to pass a group that is the
// result of resolveTarget (i.e. every replica of one service name)
// or a single runner; cross-service grouping is the orchestrator's
// job, not this helper's.
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
func (o *Orchestrator) ReloadService(ctx context.Context, group []*Runner) []error {
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
	return errs
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
				// Surface non-zero exit to the caller. The supervised
				// service is unaffected (we did not stop it), but the
				// operator needs to know the reload didn't take
				// effect, especially when zpctl is called from CI:
				// `zpctl reload nginx && deploy_next_step` should fail
				// closed if `nginx -s reload` exited 1. The error text
				// makes the "service still running" distinction
				// explicit so panic-mode rollbacks don't trigger on
				// what is fundamentally a config-syntax problem in
				// the reload payload.
				return fmt.Errorf("%s: reload_command exited %d (service still running)", name, info.ExitCode)
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
	// RestartCtx returns phase-prefixed errors ("stop:" /
	// "did not stop within timeout (state=X):" / "start:"), so the
	// historical reload message shape ("<name>: stop: ...") is
	// preserved by just prefixing the service name.
	if err := r.RestartCtx(ctx); err != nil {
		return fmt.Errorf("%s: %w", name, err)
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
	wg := o.wg
	o.mu.RUnlock()

	if len(affected) == 0 {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	o.log.Info("resource change: reloading subscribed services",
		"count", len(affected), "dimensions", change.Dimensions)

	// Track the reload-on-change goroutine in o.wg so a SIGTERM
	// during a long-running reload_command doesn't return from
	// Orchestrator.Run while the reload is still spawning children.
	// parent is the same runnerCtx Run is parked on, so cancel-on-
	// shutdown still bails the goroutine out; we just also wait for
	// the bail-out. o.wg is nil when OnResourceChange is invoked
	// outside a real Run (tests construct an Orchestrator directly
	// and drive reloadOne / OnResourceChange without Run), so guard
	// the Add accordingly.
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		if _, err := o.reloadAcrossGroups(parent, affected); err != nil {
			o.log.Warn("reload-on-change failed", "err", err)
		}
	}()
}

// reloadAcrossGroups dispatches a reload across every filename group
// represented in `runners`, parallel within group and serial between
// groups (filename order). Same shape stopAll uses, applied to
// reload semantics. Used when `zpctl reload all` runs.
//
// Returns a per-input-runner error slice in the same order as
// `runners` (nil entry on success), so callers that want to render
// per-target wire lines can do so without re-resolving names. A
// scalar errors.Join of the non-nil entries is also returned for
// callers that only care about the aggregate.
func (o *Orchestrator) reloadAcrossGroups(ctx context.Context, runners []*Runner) ([]error, error) {
	if len(runners) == 0 {
		return nil, nil
	}
	// Group by filename preserving filename-sorted order. Track each
	// runner's original index so per-runner errors can be reassembled
	// in input order on the way out.
	type indexed struct {
		idx int
		r   *Runner
	}
	groups := map[string][]indexed{}
	var order []string
	for i, r := range runners {
		fn := r.Cfg().Filename
		if _, ok := groups[fn]; !ok {
			order = append(order, fn)
		}
		groups[fn] = append(groups[fn], indexed{idx: i, r: r})
	}
	sort.Strings(order)
	perInput := make([]error, len(runners))
	for _, fn := range order {
		g := groups[fn]
		rs := make([]*Runner, len(g))
		for i := range g {
			rs[i] = g[i].r
		}
		groupErrs := o.ReloadService(ctx, rs)
		for i, e := range groupErrs {
			perInput[g[i].idx] = e
		}
	}
	var nonNil []error
	for _, e := range perInput {
		if e != nil {
			nonNil = append(nonNil, e)
		}
	}
	return perInput, errors.Join(nonNil...)
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
// Returns the diff that was actually applied alongside any error.
// On error the diff still reflects what was attempted: failed
// runners stay registered so their Run goroutine keeps tracking the
// still-live process and a follow-up reload retries them, so the
// remove/restart lists in the returned diff are still the right
// thing to show the operator. Callers that previously called
// computeDiff themselves to render the response can drop that walk
// and use this diff directly: it eliminates the racy window where
// the displayed diff and the applied diff could disagree if another
// reload landed between the two computeDiff calls.
func (o *Orchestrator) Reload(ctx context.Context, newCfg *config.Config) (reloadDiff, error) {
	o.reloadMu.Lock()
	defer o.reloadMu.Unlock()

	o.mu.RLock()
	snap := o.currentSnapshot
	o.mu.RUnlock()
	// Resolve `replicas = "auto"` in the disk-loaded config so the
	// diff machinery compares apples to apples — running auto
	// services already have a live N, and a freshly-loaded copy
	// with N=0 would otherwise look like a spec change and trigger
	// a phantom restart.
	newCfg.Services = ResolveAutoReplicasAtBoot(newCfg.Services, snap)

	// Recompute baseEnv from the new globals.Env if main.go installed
	// a builder. Without one, fall back to the existing baseEnv (tests
	// that don't care about env propagation rely on this). The current
	// resource env is captured under the same lock so a watcher-driven
	// update concurrent with reload doesn't race with the rebuild.
	o.mu.RLock()
	diff := o.computeDiffLocked(newCfg)
	builder := o.baseEnvBuilder
	resourceEnv := o.resourceEnv
	newBaseEnv := o.baseEnv
	o.mu.RUnlock()
	if builder != nil {
		newBaseEnv = builder(newCfg.Globals.Env, resourceEnv)
	}
	o.log.Info("reload", "add", len(diff.add), "remove", len(diff.remove), "restart", len(diff.restart))

	err := o.applyReloadDiff(ctx, diff, newCfg, newBaseEnv)
	return diff, err
}

// ReloadScoped applies only the add/remove/restart actions for the
// named services, leaving every other service untouched even if its
// file changed on disk. It is the engine behind `zpctl update NAME`.
//
// Two invariants distinguish it from Reload:
//
//  1. Global [env] changes are NOT applied. Propagating a globals.env
//     change restarts every reloadable service (children can't be
//     re-env'd in place), which is exactly the blast radius a scoped
//     update exists to avoid. We compute the diff against a probe
//     config whose env equals the running env so computeDiffLocked's
//     globals-propagation block is a no-op, then commit a config that
//     keeps the running Globals and swaps in only the named services'
//     specs. A later full `zpctl update` still sees and applies the
//     deferred globals change.
//  2. Only the named filenames' actions are applied; the rest of the
//     full diff is discarded. Committing the running specs for every
//     other file (rather than the disk specs) means a subsequent full
//     update still diffs unrelated out-of-band edits normally and does
//     not re-restart the services this call already updated.
//
// Returns the filtered diff that was applied. An unknown NAME yields an
// errUnknownService-wrapped error and applies nothing.
func (o *Orchestrator) ReloadScoped(ctx context.Context, newCfg *config.Config, names []string) (reloadDiff, error) {
	o.reloadMu.Lock()
	defer o.reloadMu.Unlock()

	o.mu.RLock()
	snap := o.currentSnapshot
	o.mu.RUnlock()
	newCfg.Services = ResolveAutoReplicasAtBoot(newCfg.Services, snap)

	// Build name -> filename from both the running set and the disk
	// config so an operator can name a service being added (disk only),
	// removed (running only), or changed (both).
	o.mu.RLock()
	curCfg := o.cfg
	curBaseEnv := o.baseEnv
	runFiles := map[string]string{}
	seenFile := map[string]bool{}
	for _, r := range o.runners {
		sp := r.Spec()
		if !seenFile[sp.Filename] {
			seenFile[sp.Filename] = true
			runFiles[sp.Name] = sp.Filename
		}
	}
	o.mu.RUnlock()

	diskFiles := map[string]string{}
	for _, s := range newCfg.Services {
		diskFiles[s.Name] = s.Filename
	}

	wantFiles := map[string]struct{}{}
	var unknown []string
	for _, n := range names {
		if strings.Contains(n, "/") {
			return reloadDiff{}, fmt.Errorf("update operates on whole services; drop the /N from %q", n)
		}
		if fn, ok := diskFiles[n]; ok {
			wantFiles[fn] = struct{}{}
			continue
		}
		if fn, ok := runFiles[n]; ok {
			wantFiles[fn] = struct{}{}
			continue
		}
		unknown = append(unknown, n)
	}
	if len(unknown) > 0 {
		return reloadDiff{}, fmt.Errorf("%w: %s", errUnknownService, strings.Join(unknown, ", "))
	}

	// Probe config with env forced equal to the running env, so the
	// diff carries spec-level changes only (no globals propagation).
	probe := *newCfg
	probe.Globals = curCfg.Globals
	probe.Globals.Env = curCfg.Globals.Env
	probe.Services = newCfg.Services

	o.mu.RLock()
	full := o.computeDiffLocked(&probe)
	o.mu.RUnlock()

	// Keep only the named filenames' actions.
	var diff reloadDiff
	for _, r := range full.remove {
		if _, ok := wantFiles[r.Cfg().Filename]; ok {
			diff.remove = append(diff.remove, r)
		}
	}
	for _, p := range full.restart {
		if _, ok := wantFiles[p.new.Filename]; ok {
			diff.restart = append(diff.restart, p)
		}
	}
	for _, s := range full.add {
		if _, ok := wantFiles[s.Filename]; ok {
			diff.add = append(diff.add, s)
		}
	}

	// Commit config: running services with only the named files'
	// specs swapped in / removed; Globals untouched so the deferred
	// env change still shows up on the next full update.
	svcByFile := map[string]config.Service{}
	for _, s := range curCfg.Services {
		svcByFile[s.Filename] = s
	}
	for _, r := range diff.remove {
		delete(svcByFile, r.Cfg().Filename)
	}
	for _, p := range diff.restart {
		svcByFile[p.new.Filename] = p.new
	}
	for _, s := range diff.add {
		svcByFile[s.Filename] = s
	}
	mergedServices := make([]config.Service, 0, len(svcByFile))
	for _, s := range svcByFile {
		mergedServices = append(mergedServices, s)
	}
	sort.Slice(mergedServices, func(i, j int) bool {
		return mergedServices[i].Filename < mergedServices[j].Filename
	})
	merged := *curCfg
	merged.Services = mergedServices

	o.log.Info("update (scoped)", "names", names,
		"add", len(diff.add), "remove", len(diff.remove), "restart", len(diff.restart))

	// baseEnv unchanged: globals env is not applied on a scoped update.
	err := o.applyReloadDiff(ctx, diff, &merged, curBaseEnv)
	return diff, err
}

// applyReloadDiff carries out a computed reload diff: stop removals and
// restart-olds in filename groups, expand (re)added specs into runners,
// commit the new runner set + config + baseEnv under one critical
// section, then boot the adds in a detached serial goroutine and rebind
// the exit_code_from watcher. Shared by the whole-dir Reload and the
// scoped ReloadScoped; the caller holds o.reloadMu and supplies the
// config to commit (commitCfg) and the baseEnv freshly-spawned children
// should use. Returns the joined per-action errors (nil on full
// success); failed removals stay registered for the next reload to
// retry.
func (o *Orchestrator) applyReloadDiff(ctx context.Context, diff reloadDiff, commitCfg *config.Config, newBaseEnv []string) error {
	var errs []error
	// diff.remove is built in filename-sorted order and packs every
	// replica of a filename consecutively (see computeDiffLocked).
	// Walk it as filename groups so all replicas of one logical
	// service are removed in parallel within their stop_timeout
	// rather than serially.
	for i := 0; i < len(diff.remove); {
		fn := diff.remove[i].Cfg().Filename
		j := i
		for j < len(diff.remove) && diff.remove[j].Cfg().Filename == fn {
			j++
		}
		for _, err := range o.removeServiceGroup(ctx, diff.remove[i:j]) {
			if err != nil {
				o.log.Error("reload: remove failed; runner kept registered", "err", err)
				errs = append(errs, err)
			}
		}
		i = j
	}

	// Pair restart-new specs with successful removals. p.old already
	// holds every replica of the filename being restarted, so the
	// group form fits directly. If any old replica fails to stop the
	// new spec is skipped for this reload (removeServiceGroup leaves
	// failed runners registered so the next reload's diff retries).
	var addSpecs []config.Service
	for _, p := range diff.restart {
		failed := false
		for i, err := range o.removeServiceGroup(ctx, p.old) {
			if err != nil {
				o.log.Error("reload: restart-stop failed; new spec skipped",
					"service", p.old[i].DisplayName(), "err", err)
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

	// Commit the new config + baseEnv atomically with the runner
	// registration, then boot the adds. registerAndBoot owns the lock
	// discipline and the detached serial-boot handoff shared with
	// autoscale's scaleUp.
	o.registerAndBoot(jobs, commitCfg, newBaseEnv)

	// Rebind the exit_code_from watcher: the watched service may have
	// been added in this reload, or the target name may have changed.
	o.installExitCodeWatcher()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// registerAndBoot appends jobs' runners to o.runners under the
// orchestrator lock, re-sorts, optionally commits a new config and
// baseEnv in the same critical section (so external readers see a
// consistent runners+cfg set), starts each runner's Run goroutine, and
// launches a single detached goroutine that boots the adds serially in
// filename order. Shared by applyReloadDiff (which commits cfg/baseEnv)
// and autoscale's scaleUp (which passes nil commitCfg because it only
// adds replicas to an already-committed config).
//
// Boots run on o.runnerCtx, not any caller ctx: SIGHUP-driven reloads
// come from main.go which never cancels its ctx, and the control
// server's per-request deadline is too tight for a multi-service boot.
// Tying boots to runnerCtx ties their lifetime to the supervisor.
func (o *Orchestrator) registerAndBoot(jobs []reloadBootJob, commitCfg *config.Config, newBaseEnv []string) {
	// Capture runnerCtx under the same lock as the registration so the
	// detached boot goroutine reads a properly-published value (Run is
	// the sole writer of runnerCtx; the pairing keeps it race-clean).
	o.mu.Lock()
	for _, j := range jobs {
		o.runners = append(o.runners, j.runner)
	}
	sortRunners(o.runners)
	if commitCfg != nil {
		o.cfg = commitCfg
		o.baseEnv = newBaseEnv
	}
	globals := o.cfg.Globals
	bootRoot := o.runnerCtx
	o.mu.Unlock()
	if bootRoot == nil {
		// No live Run loop (constructed-directly tests); boots still
		// need a non-nil root for runReloadBoots' ctx handling.
		bootRoot = context.Background()
	}

	// spawnRunnerGoroutine takes no orchestrator locks but pulls
	// runnerCtx via setRunCancel, so run it after the unlock.
	for _, j := range jobs {
		o.spawnRunnerGoroutine(j.runner)
	}

	if len(jobs) > 0 {
		bootJobs := append([]reloadBootJob(nil), jobs...)
		go o.runReloadBoots(bootRoot, bootJobs, globals)
	}
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
		env := service.MergeEnv(r.BaseEnv(), cfg.Env)
		if err := waitReady(ctx, cfg.Ready, env, cfg.Cwd, o.prober, o.log); err != nil {
			if cfg.Ready.OnTimeout == config.ReadyContinue {
				// See bootOne: on_timeout=continue counts as ready
				// for `zpctl ready` purposes since the operator
				// declared the probe non-blocking.
				r.MarkReady()
				o.log.Warn("reload: readiness failed; continuing per on_timeout",
					"service", name, "err", err)
			} else {
				o.log.Error("reload: added service readiness failed",
					"service", name, "err", err)
			}
		} else {
			r.MarkReady()
		}
	} else {
		r.MarkReady()
	}
}
