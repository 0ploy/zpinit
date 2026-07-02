package supervisor

import (
	"context"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/resources"
)

// autoScaleAction is one replicas="auto" service's planned rebalance:
// bring its `running` replicas to `target`. Produced by planAutoScale
// (which holds o.mu) and consumed by applyAutoScale (which holds
// neither o.mu nor reloadMu).
type autoScaleAction struct {
	spec    config.Service
	running []*Runner
	target  int
}

// planAutoScale walks every replicas="auto" service, computes the new
// target from snap, commits it to the live cfg's per-service
// Replicas.N (so subsequent reload diffs and `zpctl status` reflect
// the current target), and returns the actions needed to reach it plus
// the current baseEnv.
//
// The caller holds reloadMu across SetCurrentSnapshot AND this call so
// a concurrent Reload observes the (snapshot, Replicas.N) pair
// atomically: Reload resolves auto N from currentSnapshot, and a
// half-updated pair would make it compute a stale target. The returned
// actions are applied OUTSIDE reloadMu (see applyAutoScale) — only the
// planning needs the lock, not the teardown.
func (o *Orchestrator) planAutoScale(snap resources.Snapshot) ([]autoScaleAction, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	byFile := map[string][]*Runner{}
	for _, r := range o.runners {
		fn := r.Cfg().Filename
		byFile[fn] = append(byFile[fn], r)
	}
	var actions []autoScaleAction
	for i, svc := range o.cfg.Services {
		if !svc.Replicas.Auto {
			continue
		}
		target := ComputeAutoTarget(svc, snap)
		running := byFile[svc.Filename]
		if target == len(running) {
			continue
		}
		o.cfg.Services[i].Replicas.N = target
		actions = append(actions, autoScaleAction{
			spec:    o.cfg.Services[i],
			running: running,
			target:  target,
		})
	}
	return actions, o.baseEnv
}

// applyAutoScale executes planned scale actions. Scale-up boots
// additional replicas through the reload-boot serialization (one at a
// time, in filename order, detached); scale-down stops the
// highest-indexed extras in parallel via removeServiceGroup.
//
// Runs OUTSIDE reloadMu by design: scale-down's removeServiceGroup
// blocks on StopCtx + WaitTerminal for up to stop_timeout + reapGrace
// per group, and holding reloadMu across that would freeze every
// SIGHUP / `zpctl update` reload for the stop window on a slow-to-die
// replica. Because a Reload can restart or remove the service between
// plan and apply, both directions RE-VALIDATE against the live
// (o.cfg, o.runners) pair instead of trusting the planned action:
// scaleUp under reloadMu (registration is cheap and non-blocking),
// scaleDown under a fresh o.mu snapshot. Applying the stale plan
// verbatim could register old-spec runners with duplicate replica
// indices, a state the index-based victim selection can never undo.
func (o *Orchestrator) applyAutoScale(ctx context.Context, actions []autoScaleAction, baseEnv []string) {
	for _, a := range actions {
		if a.target > len(a.running) {
			o.scaleUp(a.spec.Filename, baseEnv)
		} else {
			o.scaleDown(ctx, a.spec.Filename)
		}
	}
}

// currentAutoState returns the live spec and running replicas for an
// auto service by filename. ok is false when the service is gone or
// no longer auto (a Reload interleaved since the plan); target is the
// committed Replicas.N, which the scaler owns.
func (o *Orchestrator) currentAutoState(filename string) (spec config.Service, running []*Runner, ok bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	found := false
	for _, s := range o.cfg.Services {
		if s.Filename == filename {
			spec, found = s, true
			break
		}
	}
	if !found || !spec.Replicas.Auto {
		return config.Service{}, nil, false
	}
	for _, r := range o.runners {
		if r.Cfg().Filename == filename {
			running = append(running, r)
		}
	}
	return spec, running, true
}

// scaleUp brings the named auto service up to its committed target,
// allocating the smallest free replica indices. Registration and the
// detached, reloadBootMu-serialized boot are handled by
// registerAndBoot (shared with reload); commitCfg is nil because
// autoscale only adds replicas to the already-committed config (the
// new Replicas.N was written by planAutoScale).
//
// Holds reloadMu: the plan this action came from was made under
// reloadMu but is applied outside it, and a Reload restarting the
// service in between would otherwise race this registration into
// duplicate (old-spec, colliding-index) runners. Registration is
// non-blocking, so the reload stall is microseconds; only scale-DOWN
// must stay outside the lock. Indices come from the actual free set,
// not len(running): a partial scale-down failure leaves holes, and
// len-based allocation would mint an index that is still alive
// (colliding {index} log paths, ambiguous svc/N targets).
func (o *Orchestrator) scaleUp(filename string, baseEnv []string) {
	o.reloadMu.Lock()
	defer o.reloadMu.Unlock()

	spec, running, ok := o.currentAutoState(filename)
	if !ok {
		o.log.Info("autoscale: service changed during rebalance; scale-up dropped", "file", filename)
		return
	}
	target := spec.Replicas.N
	if len(running) >= target {
		return
	}
	used := make(map[int]bool, len(running))
	for _, r := range running {
		used[r.ReplicaIndex()] = true
	}
	o.log.Info("autoscale: scaling up",
		"service", spec.Name, "from", len(running), "to", target)
	jobs := make([]reloadBootJob, 0, target-len(running))
	idx := 0
	for n := target - len(running); n > 0; n-- {
		for used[idx] {
			idx++
		}
		used[idx] = true
		perReplica := spec
		// Always replicated: only auto services reach scaleUp, and auto
		// replicas are expanded regardless of the current target (see
		// expandServiceToRunners).
		perReplica.Log.Stdout = replicaLogPath(spec.Log.Stdout, idx, true)
		perReplica.Log.Stderr = replicaLogPath(spec.Log.Stderr, idx, true)
		env := composeReplicaEnv(baseEnv, idx, true)
		// NewRunnerForReplica keeps spec = the unmodified service-
		// level config for reload-diff equality; cfg carries the
		// per-replica log/env rewrites used at spawn time.
		r := NewRunnerForReplica(perReplica, spec, env, idx, o.spawner, o.clock, o.log)
		jobs = append(jobs, reloadBootJob{cfg: r.Cfg(), runner: r})
	}
	if err := o.registerAndBoot(jobs, nil, nil); err != nil {
		o.log.Warn("autoscale: scale-up refused", "service", spec.Name, "err", err)
	}
}

// scaleDown stops the named service's runners whose replicaIndex is
// >= the committed target and removes them from the registry. Uses
// removeServiceGroup so stop_timeout and SIGKILL escalation behave
// the same as for a reload-driven removal. Victims come from a fresh
// snapshot, not the planned action: a Reload interleaving since the
// plan may have already stopped (and deregistered) the planned
// runners, and StopCtx on a dead runner parks for the full stop
// budget.
func (o *Orchestrator) scaleDown(ctx context.Context, filename string) {
	spec, running, ok := o.currentAutoState(filename)
	if !ok {
		o.log.Info("autoscale: service changed during rebalance; scale-down dropped", "file", filename)
		return
	}
	target := spec.Replicas.N
	var victims []*Runner
	for _, r := range running {
		if r.ReplicaIndex() >= target {
			victims = append(victims, r)
		}
	}
	if len(victims) == 0 {
		return
	}
	o.log.Info("autoscale: scaling down",
		"service", spec.Name,
		"from", len(running), "to", target,
		"removing", len(victims))
	for _, err := range o.removeServiceGroup(ctx, victims) {
		if err != nil {
			o.log.Warn("autoscale: replica did not stop cleanly; runner kept registered for next pass",
				"err", err)
		}
	}
}
