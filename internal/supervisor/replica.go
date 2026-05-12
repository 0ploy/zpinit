package supervisor

import (
	"log/slog"
	"strconv"
	"strings"

	"github.com/0ploy/zpinit/internal/config"
)

// replicaLogPath delegates to config.ReplicaLogPath so the supervisor
// and doctor share one expansion rule.
func replicaLogPath(spec string, idx, total int) string {
	return config.ReplicaLogPath(spec, idx, total)
}

// expandServiceToRunners turns a single config.Service spec into N
// Runners, one per replica. For svc.Replicas <= 1 it returns a single
// runner whose log paths and env are byte-for-byte what they would
// have been before replicas existed (zero-regression contract for
// non-replicated services).
//
// Per-replica state lives on the Runner: the spec's log paths are
// rewritten to per-replica files and ZPINIT_REPLICA_INDEX is injected
// into the spawn env. The original svc is kept by the orchestrator
// for diff purposes (servicesEqual compares specs, not per-replica
// copies).
func expandServiceToRunners(svc config.Service, baseEnv []string, spawner Spawner, clock Clock, log *slog.Logger) []*Runner {
	n := svc.Replicas
	if n < 1 {
		n = 1
	}
	out := make([]*Runner, n)
	for i := 0; i < n; i++ {
		perReplica := svc
		perReplica.Log.Stdout = replicaLogPath(svc.Log.Stdout, i, n)
		perReplica.Log.Stderr = replicaLogPath(svc.Log.Stderr, i, n)
		env := composeReplicaEnv(baseEnv, i, n)
		r := NewRunner(perReplica, env, i, spawner, clock, log)
		// Reset spec to the original (NewRunner defaults spec=cfg);
		// servicesEqual compares specs, and the per-replica log
		// rewriting must not show up as a phantom diff on reload.
		r.spec = svc
		out[i] = r
	}
	return out
}

// composeReplicaEnv produces the env slice for replica idx of a
// services with `total` replicas. For total <= 1 it returns base
// unchanged (no ZPINIT_REPLICA_INDEX injection — keeps the env
// footprint identical to today for non-replicated services).
//
// If base already contains a ZPINIT_REPLICA_INDEX entry (e.g. an
// operator put it in globals.Env), the slot is replaced with the
// per-replica value rather than appended.
func composeReplicaEnv(base []string, idx, total int) []string {
	if total <= 1 {
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
