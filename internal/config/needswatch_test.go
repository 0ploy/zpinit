package config

import (
	"testing"

	"github.com/0ploy/zpinit/internal/resources"
)

func TestNeedsResourceWatch(t *testing.T) {
	static := Service{Name: "a", Filename: "10_a.toml", Replicas: Replicas{N: 1}}
	replicated := Service{Name: "b", Filename: "20_b.toml", Replicas: Replicas{N: 4}}
	auto := Service{Name: "c", Filename: "30_c.toml", Replicas: Replicas{Auto: true, N: 2}}
	onChange := Service{
		Name: "d", Filename: "40_d.toml", Replicas: Replicas{N: 1},
		ReloadOnChange: []string{resources.DimCPU},
	}
	// An explicit empty list is the documented opt-OUT for an auto
	// service; on a static service it must not arm the watcher either.
	emptyOnChange := Service{
		Name: "e", Filename: "50_e.toml", Replicas: Replicas{N: 1},
		ReloadOnChange: []string{},
	}

	tests := []struct {
		name string
		svcs []Service
		want bool
	}{
		{"no services at all", nil, false},
		{"static only", []Service{static}, false},
		{"static replicas > 1 is not auto", []Service{static, replicated}, false},
		{"reload_on_change = [] does not arm", []Service{emptyOnChange}, false},
		{"one auto service arms it", []Service{static, auto}, true},
		{"one reload_on_change service arms it", []Service{static, onChange}, true},
		{"auto last in the list is still found", []Service{static, replicated, auto}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Services: tc.svcs}
			if got := c.NeedsResourceWatch(); got != tc.want {
				t.Errorf("NeedsResourceWatch() = %v, want %v", got, tc.want)
			}
		})
	}
}

// NewEmpty is the missing-config-dir fallback; it must not arm the
// watcher (nil Services would otherwise be an easy nil-deref).
func TestNeedsResourceWatch_EmptyConfig(t *testing.T) {
	if NewEmpty("/etc/zpinit").NeedsResourceWatch() {
		t.Error("an empty config must not need the resource watcher")
	}
}
