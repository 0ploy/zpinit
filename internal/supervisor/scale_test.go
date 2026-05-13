package supervisor

import (
	"testing"

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
