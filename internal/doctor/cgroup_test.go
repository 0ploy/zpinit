package doctor

import (
	"strings"
	"testing"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/resources"
)

func autoCfg(auto bool) *config.Config {
	s := config.Service{Name: "worker", Filename: "10_worker.toml"}
	if auto {
		s.Replicas = config.Replicas{Auto: true, N: 2}
	} else {
		s.Replicas = config.Replicas{N: 2}
	}
	return &config.Config{Services: []config.Service{s}}
}

func TestCheckCgroup(t *testing.T) {
	tests := []struct {
		name     string
		snap     resources.Snapshot
		auto     bool
		want     Status
		contains string
	}{
		{
			name: "resolved is OK",
			snap: resources.Snapshot{CPUCount: 2, MemoryBytes: 536870912, CgroupResolved: true},
			want: StatusOK, contains: "resolved this container's cgroup",
		},
		{
			name: "resolved stays OK even with an auto service",
			snap: resources.Snapshot{CPUCount: 2, CgroupResolved: true},
			auto: true, want: StatusOK,
		},
		{
			// Static replicas: the figures are wrong and the env vars
			// mislead, but nothing scales off them automatically.
			name: "unresolved without auto is a warning",
			snap: resources.Snapshot{CPUCount: 96, CgroupResolved: false},
			want: StatusWarn, contains: "may be the host's",
		},
		{
			// replicas = "auto" turns the bad number into 96 processes.
			name: "unresolved with an auto service is a failure",
			snap: resources.Snapshot{CPUCount: 96, CgroupResolved: false},
			auto: true, want: StatusFail, contains: `service "worker" uses replicas = "auto"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkCgroup(autoCfg(tc.auto), tc.snap)
			if len(got) != 1 {
				t.Fatalf("got %d checks, want 1", len(got))
			}
			if got[0].Status != tc.want {
				t.Errorf("status = %v, want %v (detail: %s)", got[0].Status, tc.want, got[0].Detail)
			}
			if tc.contains != "" && !strings.Contains(got[0].Detail, tc.contains) {
				t.Errorf("detail = %q, want it to contain %q", got[0].Detail, tc.contains)
			}
		})
	}
}

func TestMemoryDisplay(t *testing.T) {
	if got := memoryDisplay(0); got != "no memory limit" {
		t.Errorf("memoryDisplay(0) = %q", got)
	}
	if got := memoryDisplay(uint64(1) << 30); got != "1.00 GiB" {
		t.Errorf("memoryDisplay(1GiB) = %q", got)
	}
}
