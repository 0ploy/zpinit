package supervisor

import (
	"log/slog"
	"strconv"
	"strings"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/resources"
)

// ComputeAutoTarget returns the target replica count for a service
// declared as `replicas = "auto"`. Takes the floor of the detected
// CPU budget (subtracting reservations has already happened by the
// time the Snapshot reaches here) and clamps to
// [ReplicasMin, ReplicasMax] when bounds are set. Replicas_min
// acts as a floor that can drive the count *above* the natural CPU
// count — useful for I/O-bound queue workers.
//
// Always returns at least 1: a service that exists must have at
// least one runner.
func ComputeAutoTarget(s config.Service, snap resources.Snapshot) int {
	natural := snap.CPUCount
	if natural < 1 {
		natural = 1
	}
	target := natural
	if s.ReplicasMin > 0 && target < s.ReplicasMin {
		target = s.ReplicasMin
	}
	if s.ReplicasMax > 0 && target > s.ReplicasMax {
		target = s.ReplicasMax
	}
	// The fork-bomb guard applies to auto mode too: without a
	// replicas_max, an unquota'd container on a 96-core host would
	// otherwise boot 96 replicas per auto service. replicas_min is
	// validated <= MaxReplicas at load time, so clamping last is safe.
	if target > config.MaxReplicas {
		target = config.MaxReplicas
	}
	if target < 1 {
		target = 1
	}
	return target
}

// ResolveAutoReplicasAtBoot sets the initial N for every auto
// service in cfg.Services based on the boot-time Snapshot. Called
// from main.go after Detect+reserves but before NewOrchestrator,
// so that the orchestrator's expandServiceToRunners spawns the
// right number of children. Static services are left untouched.
func ResolveAutoReplicasAtBoot(services []config.Service, snap resources.Snapshot) []config.Service {
	out := make([]config.Service, len(services))
	for i, s := range services {
		if s.Replicas.Auto {
			s.Replicas.N = ComputeAutoTarget(s, snap)
		}
		out[i] = s
	}
	return out
}

// replicaLogPath delegates to config.ReplicaLogPath so the supervisor
// and doctor share one expansion rule.
func replicaLogPath(spec string, idx int, replicated bool) string {
	return config.ReplicaLogPath(spec, idx, replicated)
}

// expandServiceToRunners turns a single config.Service spec into N
// Runners, one per replica. For a static `replicas = 1` (or absent)
// it returns a single runner whose log paths and env are
// byte-for-byte what they would have been before replicas existed
// (zero-regression contract for non-replicated services).
//
// `replicas = "auto"` services are ALWAYS treated as replicated,
// even when the resolved target is 1: replica 0 gets
// ZPINIT_REPLICA_INDEX=0 and {index} expansion just like it would at
// N=4, so a later scale-up doesn't leave replica 0 as the odd one
// out with an index-less env and a literal `{index}` log path.
//
// Per-replica state lives on the Runner: the spec's log paths are
// rewritten to per-replica files and ZPINIT_REPLICA_INDEX is injected
// into the spawn env. The original svc is kept by the orchestrator
// for diff purposes (servicesEqual compares specs, not per-replica
// copies).
func expandServiceToRunners(svc config.Service, baseEnv []string, spawner Spawner, clock Clock, log *slog.Logger) []*Runner {
	n := svc.Replicas.N
	if n < 1 {
		n = 1
	}
	replicated := svc.Replicas.Auto || n > 1
	out := make([]*Runner, n)
	for i := 0; i < n; i++ {
		perReplica := svc
		perReplica.Log.Stdout = replicaLogPath(svc.Log.Stdout, i, replicated)
		perReplica.Log.Stderr = replicaLogPath(svc.Log.Stderr, i, replicated)
		env := composeReplicaEnv(baseEnv, i, replicated)
		// NewRunnerForReplica keeps spec = svc (the unmodified
		// service-level config) while cfg carries the per-replica
		// log-path and env rewrites used at spawn time. servicesEqual
		// compares specs, so this prevents per-replica rewrites from
		// looking like a config change on every reload.
		out[i] = NewRunnerForReplica(perReplica, svc, env, i, spawner, clock, log)
	}
	return out
}

// composeReplicaEnv produces the env slice for replica idx of a
// replicated service. For non-replicated services (static
// replicas <= 1) it returns base unchanged (no ZPINIT_REPLICA_INDEX
// injection; keeps the env footprint identical to today).
// Auto services pass replicated=true even at a resolved N of 1; see
// expandServiceToRunners.
//
// If base already contains a ZPINIT_REPLICA_INDEX entry (e.g. an
// operator put it in globals.Env), the slot is replaced with the
// per-replica value rather than appended.
func composeReplicaEnv(base []string, idx int, replicated bool) []string {
	if !replicated {
		return base
	}
	out := make([]string, 0, len(base)+1)
	seen := false
	for _, e := range base {
		if strings.HasPrefix(e, "ZPINIT_REPLICA_INDEX=") {
			out = append(out, "ZPINIT_REPLICA_INDEX="+strconv.Itoa(idx))
			seen = true
			continue
		}
		out = append(out, e)
	}
	if !seen {
		out = append(out, "ZPINIT_REPLICA_INDEX="+strconv.Itoa(idx))
	}
	return out
}
