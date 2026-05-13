package supervisor

import (
	"context"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/resources"
)

// scaleAutoServices walks every service declared replicas="auto"
// and brings the live runner count up or down to match the new
// target computed from snap. Scale-up boots additional replicas
// through the reload-boot serialization (one at a time, in
// filename order); scale-down stops the highest-indexed extras in
// parallel via removeServiceGroup. The live cfg's per-service
// Replicas.N is updated so subsequent reload diffs and zpctl
// status reflect the current target.
//
// Called from OnResourceChange after SetResourceEnv. Errors are
// logged; we don't propagate them because there's no client to
// receive them.
func (o *Orchestrator) scaleAutoServices(ctx context.Context, snap resources.Snapshot) {
	type action struct {
		spec    config.Service
		running []*Runner
		target  int
	}
	var actions []action

	o.mu.Lock()
	byFile := map[string][]*Runner{}
	for _, r := range o.runners {
		fn := r.Cfg().Filename
		byFile[fn] = append(byFile[fn], r)
	}
	for i, svc := range o.cfg.Services {
		if !svc.Replicas.Auto {
			continue
		}
		target := ComputeAutoTarget(svc, snap)
		running := byFile[svc.Filename]
		if target == len(running) {
			continue
		}
		// Update the live cfg's N so future reload diffs and the
		// `zpctl status` / update output reflect the current target.
		o.cfg.Services[i].Replicas.N = target
		actions = append(actions, action{
			spec:    o.cfg.Services[i],
			running: running,
			target:  target,
		})
	}
	baseEnv := o.baseEnv
	bootRoot := o.runnerCtx
	o.mu.Unlock()
	if bootRoot == nil {
		bootRoot = context.Background()
	}

	for _, a := range actions {
		if a.target > len(a.running) {
			o.scaleUp(bootRoot, a.spec, len(a.running), a.target, baseEnv)
		} else {
			o.scaleDown(ctx, a.running, a.target)
		}
	}
}

// scaleUp spawns the new replicas for indices [from, to). Boot is
// detached and serialized on reloadBootMu so back-to-back resource
// commits don't interleave their boots. The new runners are
// registered synchronously so a subsequent scaleAutoServices call
// sees the correct count.
func (o *Orchestrator) scaleUp(bootRoot context.Context, spec config.Service, from, to int, baseEnv []string) {
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
		r := NewRunner(perReplica, env, i, o.spawner, o.clock, o.log)
		// Keep the unmodified spec for reload-diff equality.
		r.spec = spec
		jobs = append(jobs, reloadBootJob{cfg: r.Cfg(), runner: r})
	}

	o.mu.Lock()
	for _, j := range jobs {
		o.runners = append(o.runners, j.runner)
	}
	sortRunners(o.runners)
	globals := o.cfg.Globals
	o.mu.Unlock()

	for _, j := range jobs {
		o.spawnRunnerGoroutine(j.runner)
	}
	go o.runReloadBoots(bootRoot, jobs, globals)
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
