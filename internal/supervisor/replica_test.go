package supervisor

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/0ploy/zpinit/internal/config"
)

func TestReplicaLogPath_SingleReplicaNoRewrite(t *testing.T) {
	if got := replicaLogPath("/var/log/x.log", 0, false); got != "/var/log/x.log" {
		t.Errorf("got %q, want unchanged path for non-replicated", got)
	}
}

func TestReplicaLogPath_InheritUnchanged(t *testing.T) {
	if got := replicaLogPath("inherit", 1, true); got != "inherit" {
		t.Errorf("got %q, want inherit unchanged", got)
	}
	if got := replicaLogPath("", 1, true); got != "" {
		t.Errorf("got %q, want empty unchanged", got)
	}
}

func TestReplicaLogPath_ExplicitPlaceholder(t *testing.T) {
	if got := replicaLogPath("/var/log/consumer-{index}.log", 2, true); got != "/var/log/consumer-2.log" {
		t.Errorf("got %q, want /var/log/consumer-2.log", got)
	}
	if got := replicaLogPath("/logs/{index}/app.log", 0, true); got != "/logs/0/app.log" {
		t.Errorf("got %q, want /logs/0/app.log", got)
	}
}

func TestReplicaLogPath_NoPlaceholderIsShared(t *testing.T) {
	// All replicas share the same path when no {index} placeholder is
	// present. Linux O_APPEND keeps concurrent line-sized writes from
	// tearing.
	if got := replicaLogPath("/var/log/consumer.log", 2, true); got != "/var/log/consumer.log" {
		t.Errorf("got %q, want unchanged (shared file)", got)
	}
	if got := replicaLogPath("/var/log/consumer", 1, true); got != "/var/log/consumer" {
		t.Errorf("got %q, want unchanged (shared file)", got)
	}
}

func TestComposeReplicaEnv_NoInjectionForSingle(t *testing.T) {
	base := []string{"PATH=/usr/bin", "FOO=bar"}
	got := composeReplicaEnv(base, 0, false)
	if !reflect.DeepEqual(got, base) {
		t.Errorf("got %v, want unchanged base for non-replicated", got)
	}
}

func TestComposeReplicaEnv_AppendsIndex(t *testing.T) {
	base := []string{"PATH=/usr/bin", "FOO=bar"}
	got := composeReplicaEnv(base, 2, true)
	want := []string{"PATH=/usr/bin", "FOO=bar", "ZPINIT_REPLICA_INDEX=2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestComposeReplicaEnv_ReplacesExistingIndex(t *testing.T) {
	base := []string{"PATH=/usr/bin", "ZPINIT_REPLICA_INDEX=99", "FOO=bar"}
	got := composeReplicaEnv(base, 3, true)
	want := []string{"PATH=/usr/bin", "ZPINIT_REPLICA_INDEX=3", "FOO=bar"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandServiceToRunners_SingleReplica(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := config.Service{
		Name:     "api",
		Filename: "10_api.toml",
		Replicas: config.Replicas{N: 1},
		Log:      config.Logging{Stdout: "/var/log/api.log", Stderr: "/var/log/api.err"},
	}
	rs := expandServiceToRunners(svc, []string{"FOO=bar"}, nil, nil, log)
	if len(rs) != 1 {
		t.Fatalf("got %d runners, want 1", len(rs))
	}
	r := rs[0]
	if got := r.ReplicaIndex(); got != 0 {
		t.Errorf("ReplicaIndex = %d, want 0", got)
	}
	if got := r.DisplayName(); got != "api" {
		t.Errorf("DisplayName = %q, want api", got)
	}
	// Log paths must be untouched for single-replica.
	if got := r.Cfg().Log.Stdout; got != "/var/log/api.log" {
		t.Errorf("Log.Stdout = %q, want unchanged", got)
	}
	// Env must be the unmodified base (no ZPINIT_REPLICA_INDEX injection).
	if !reflect.DeepEqual(r.baseEnv, []string{"FOO=bar"}) {
		t.Errorf("baseEnv = %v, want unchanged base", r.baseEnv)
	}
}

func TestExpandServiceToRunners_MultiReplicaSpawnsN(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := config.Service{
		Name:     "consumer",
		Filename: "30_consumer.toml",
		Replicas: config.Replicas{N: 3},
		Log:      config.Logging{Stdout: "/var/log/consumer.log", Stderr: "/var/log/consumer.err"},
	}
	rs := expandServiceToRunners(svc, []string{"FOO=bar"}, nil, nil, log)
	if len(rs) != 3 {
		t.Fatalf("got %d runners, want 3", len(rs))
	}
	// No {index} placeholder: all replicas share the same path.
	for i, r := range rs {
		if got := r.ReplicaIndex(); got != i {
			t.Errorf("[%d] ReplicaIndex = %d, want %d", i, got, i)
		}
		if got := r.DisplayName(); got != "consumer/"+string(rune('0'+i)) {
			t.Errorf("[%d] DisplayName = %q", i, got)
		}
		if got := r.Cfg().Log.Stdout; got != "/var/log/consumer.log" {
			t.Errorf("[%d] Log.Stdout = %q, want shared /var/log/consumer.log", i, got)
		}
		// Each replica must carry its own index in env.
		envFound := false
		for _, e := range r.baseEnv {
			if e == "ZPINIT_REPLICA_INDEX="+string(rune('0'+i)) {
				envFound = true
				break
			}
		}
		if !envFound {
			t.Errorf("[%d] missing ZPINIT_REPLICA_INDEX=%d in env: %v", i, i, r.baseEnv)
		}
	}
}

func TestExpandServiceToRunners_AutoSingleReplicaStillExpands(t *testing.T) {
	// An auto service resolved to N=1 must behave exactly like any
	// other replica set: index env injected, {index} expanded. A later
	// 1 -> 4 scale-up otherwise leaves replica 0 with an index-less
	// env and a literal {index} log path while 1..3 are correct.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := config.Service{
		Name:     "worker",
		Filename: "40_worker.toml",
		Replicas: config.Replicas{N: 1, Auto: true},
		Log:      config.Logging{Stdout: "/var/log/worker-{index}.log", Stderr: "inherit"},
	}
	rs := expandServiceToRunners(svc, []string{"FOO=bar"}, nil, nil, log)
	if len(rs) != 1 {
		t.Fatalf("got %d runners, want 1", len(rs))
	}
	r := rs[0]
	if got := r.Cfg().Log.Stdout; got != "/var/log/worker-0.log" {
		t.Errorf("Log.Stdout = %q, want /var/log/worker-0.log", got)
	}
	want := []string{"FOO=bar", "ZPINIT_REPLICA_INDEX=0"}
	if !reflect.DeepEqual(r.baseEnv, want) {
		t.Errorf("baseEnv = %v, want %v", r.baseEnv, want)
	}
	if got := r.DisplayName(); got != "worker/0" {
		t.Errorf("DisplayName = %q, want worker/0", got)
	}
}

func TestExpandServiceToRunners_PlaceholderHonored(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := config.Service{
		Name:     "consumer",
		Filename: "30_consumer.toml",
		Replicas: config.Replicas{N: 2},
		Log:      config.Logging{Stdout: "/var/log/consumer-{index}.log", Stderr: "inherit"},
	}
	rs := expandServiceToRunners(svc, nil, nil, nil, log)
	if rs[0].Cfg().Log.Stdout != "/var/log/consumer-0.log" {
		t.Errorf("rs[0] stdout = %q", rs[0].Cfg().Log.Stdout)
	}
	if rs[1].Cfg().Log.Stdout != "/var/log/consumer-1.log" {
		t.Errorf("rs[1] stdout = %q", rs[1].Cfg().Log.Stdout)
	}
	// inherit must be preserved verbatim.
	for i, r := range rs {
		if r.Cfg().Log.Stderr != "inherit" {
			t.Errorf("rs[%d] stderr = %q, want inherit", i, r.Cfg().Log.Stderr)
		}
	}
}
