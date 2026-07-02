package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/resources"
)

func TestComputeAutoTarget_NoBounds(t *testing.T) {
	s := config.Service{Replicas: config.Replicas{Auto: true}}
	got := ComputeAutoTarget(s, resources.Snapshot{CPUCount: 4})
	if got != 4 {
		t.Errorf("got %d, want 4 (natural)", got)
	}
}

func TestComputeAutoTarget_MinFloorAboveCPU(t *testing.T) {
	s := config.Service{
		Replicas:    config.Replicas{Auto: true},
		ReplicasMin: 16,
	}
	got := ComputeAutoTarget(s, resources.Snapshot{CPUCount: 2})
	if got != 16 {
		t.Errorf("got %d, want 16 (min floor over natural)", got)
	}
}

func TestComputeAutoTarget_MaxCeiling(t *testing.T) {
	s := config.Service{
		Replicas:    config.Replicas{Auto: true},
		ReplicasMax: 4,
	}
	got := ComputeAutoTarget(s, resources.Snapshot{CPUCount: 32})
	if got != 4 {
		t.Errorf("got %d, want 4 (max ceiling)", got)
	}
}

func TestComputeAutoTarget_GlobalCapWithoutMax(t *testing.T) {
	// The MaxReplicas fork-bomb guard applies to auto mode too: no
	// replicas_max on a big unquota'd host must not boot per-core
	// replicas past the cap (docs promise the "64 typo guard").
	s := config.Service{Replicas: config.Replicas{Auto: true}}
	got := ComputeAutoTarget(s, resources.Snapshot{CPUCount: 128})
	if got != config.MaxReplicas {
		t.Errorf("got %d, want %d (global cap)", got, config.MaxReplicas)
	}
}

func TestComputeAutoTarget_AlwaysAtLeastOne(t *testing.T) {
	s := config.Service{Replicas: config.Replicas{Auto: true}}
	got := ComputeAutoTarget(s, resources.Snapshot{CPUCount: 0})
	if got != 1 {
		t.Errorf("got %d, want 1 (floor on CPUCount=0)", got)
	}
}

func TestResolveAutoReplicasAtBoot(t *testing.T) {
	in := []config.Service{
		{Name: "static", Replicas: config.Replicas{N: 3}},
		{Name: "auto-bound", Replicas: config.Replicas{Auto: true}, ReplicasMax: 2},
		{Name: "auto-unbound", Replicas: config.Replicas{Auto: true}},
	}
	out := ResolveAutoReplicasAtBoot(in, resources.Snapshot{CPUCount: 8})
	if out[0].Replicas.N != 3 || out[0].Replicas.Auto {
		t.Errorf("static service got rewritten: %+v", out[0].Replicas)
	}
	if !out[1].Replicas.Auto || out[1].Replicas.N != 2 {
		t.Errorf("auto-bound: got %+v, want auto with N=2 (max)", out[1].Replicas)
	}
	if !out[2].Replicas.Auto || out[2].Replicas.N != 8 {
		t.Errorf("auto-unbound: got %+v, want auto with N=8 (natural)", out[2].Replicas)
	}
}

// scaleTestOrch builds an orchestrator with a live runnerCtx/wg (the
// shape Run publishes) so scaleUp's registerAndBoot path works
// without a full Run loop. Cleanup cancels the runner goroutines.
func scaleTestOrch(t *testing.T, svc config.Service, indices []int) *Orchestrator {
	t.Helper()
	o := &Orchestrator{
		log: testLog(),
		cfg: &config.Config{
			Services: []config.Service{svc},
			Globals: config.Globals{
				ExitCodeFrom: "default",
				BootTimeout:  config.Duration(time.Second),
			},
		},
		spawner: func(config.Service, []string) (Process, error) {
			return newFakeProcess(7000), nil
		},
		clock: newFakeClock(time.Now()),
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	o.runnerCtx = ctx
	o.wg = &wg
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	for _, idx := range indices {
		r := NewRunnerForReplica(svc, svc, nil, idx, o.spawner, o.clock, o.log)
		o.runners = append(o.runners, r)
		o.spawnRunnerGoroutine(r)
	}
	sortRunners(o.runners)
	return o
}

// TestScaleUp_FillsIndexHoles pins free-index allocation: a partial
// scale-down failure leaves holes in the index set, and len-based
// allocation would mint an index that is still alive (colliding
// {index} log paths, ambiguous svc/N targets).
func TestScaleUp_FillsIndexHoles(t *testing.T) {
	svc := config.Service{
		Name: "w", Filename: "10_w.toml", Command: []string{"x"},
		Replicas: config.Replicas{Auto: true, N: 5}, StopSignal: "TERM",
	}
	o := scaleTestOrch(t, svc, []int{0, 1, 3})

	o.scaleUp("10_w.toml", nil)

	snap := o.snapshotRunners()
	if len(snap) != 5 {
		t.Fatalf("runner count = %d, want 5", len(snap))
	}
	seen := map[int]bool{}
	for _, r := range snap {
		if seen[r.ReplicaIndex()] {
			t.Fatalf("duplicate replica index %d", r.ReplicaIndex())
		}
		seen[r.ReplicaIndex()] = true
	}
	for want := 0; want < 5; want++ {
		if !seen[want] {
			t.Errorf("index %d missing; got set %v", want, seen)
		}
	}
}

// TestScaleUp_DroppedWhenServiceChanged pins the plan/apply
// revalidation: a Reload that removed (or de-auto'd) the service
// between planAutoScale and applyAutoScale must void the action
// instead of registering runners for a service the operator deleted.
func TestScaleUp_DroppedWhenServiceChanged(t *testing.T) {
	svc := config.Service{
		Name: "w", Filename: "10_w.toml", Command: []string{"x"},
		Replicas: config.Replicas{Auto: true, N: 4}, StopSignal: "TERM",
	}
	o := scaleTestOrch(t, svc, nil)
	// Simulate the interleaved Reload: service gone from the
	// committed config before the planned action is applied.
	o.mu.Lock()
	o.cfg = &config.Config{Globals: o.cfg.Globals}
	o.mu.Unlock()

	o.scaleUp("10_w.toml", nil)

	if n := len(o.snapshotRunners()); n != 0 {
		t.Errorf("scale-up registered %d runners for a removed service, want 0", n)
	}
}
