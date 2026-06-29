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
// replica. Releasing the lock between plan and apply is safe because
// Reload never diffs an auto service's replica count (servicesEqual
// ignores auto N; the scaler owns it), so a Reload that interleaves
// here can't race the scaler over the same runners.
func (o *Orchestrator) applyAutoScale(ctx context.Context, actions []autoScaleAction, baseEnv []string) {
	for _, a := range actions {
		if a.target > len(a.running) {
			o.scaleUp(a.spec, len(a.running), a.target, baseEnv)
		} else {
			o.scaleDown(ctx, a.running, a.target)
		}
	}
}

// scaleUp spawns the new replicas for indices [from, to). Registration
// and the detached, reloadBootMu-serialized boot are handled by
// registerAndBoot (shared with reload); commitCfg is nil because
// autoscale only adds replicas to the already-committed config (the
// new Replicas.N was written by planAutoScale).
func (o *Orchestrator) scaleUp(spec config.Service, from, to int, baseEnv []string) {
	if to <= from {
		return
	}
	o.log.Info("autoscale: scaling up",
		"service", spec.Name, "from", from, "to", to)
	jobs := make([]reloadBootJob, 0, to-from)
	for i := from; i < to; i++ {
		perReplica := spec
		perReplica.Log.Stdout = replicaLogPath(spec.Log.Stdout, i, to)
		perReplica.Log.Stderr = replicaLogPath(spec.Log.Stderr, i, to)
		env := composeReplicaEnv(baseEnv, i, to)
		// NewRunnerForReplica keeps spec = the unmodified service-
		// level config for reload-diff equality; cfg carries the
		// per-replica log/env rewrites used at spawn time.
		r := NewRunnerForReplica(perReplica, spec, env, i, o.spawner, o.clock, o.log)
		jobs = append(jobs, reloadBootJob{cfg: r.Cfg(), runner: r})
	}
	o.registerAndBoot(jobs, nil, nil)
}

// scaleDown stops the runners whose replicaIndex is >= target and
// removes them from the registry. Uses removeServiceGroup so
// stop_timeout and SIGKILL escalation behave the same as for a
// reload-driven removal.
func (o *Orchestrator) scaleDown(ctx context.Context, running []*Runner, target int) {
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
		"service", running[0].Cfg().Name,
		"from", len(running), "to", target,
		"removing", len(victims))
	for _, err := range o.removeServiceGroup(ctx, victims) {
		if err != nil {
			o.log.Warn("autoscale: replica did not stop cleanly; runner kept registered for next pass",
				"err", err)
		}
	}
}
