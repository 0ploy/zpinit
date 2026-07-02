package config

import (
	"strconv"
	"strings"
)

// ReplicaLogPath rewrites a [log] path spec for one replica.
//
//	!replicated                -> spec verbatim (no rewriting)
//	spec == "" or "inherit"    -> spec verbatim
//	spec contains "{index}"    -> placeholder replaced with idx
//	otherwise                  -> spec verbatim; all replicas share the file
//
// replicated is true for `replicas > 1` AND for `replicas = "auto"`
// regardless of the resolved count: an auto service that lands on
// N=1 today can scale to N=4 on the next resource change, and its
// replica 0 must behave identically either way (a literal `{index}`
// left in the path would otherwise collide with the expanded paths
// of replicas 1..3). Static `replicas = 1` stays unexpanded
// (zero-regression contract for non-replicated services).
//
// Shared file is the default. Linux O_APPEND is atomic for writes
// below PIPE_BUF (typically 4096 bytes), so concurrent appends from
// N replicas don't tear at line boundaries for normal log output.
// Operators who want per-replica files opt in via `{index}` in the
// path: `/var/log/consumer-{index}.log` produces
// `/var/log/consumer-0.log`, etc.
//
// Centralized in the config package so the supervisor (per-replica
// spawn) and doctor (pre-flight preview) share one rule; a change to
// the placeholder syntax now happens in one place.
func ReplicaLogPath(spec string, idx int, replicated bool) string {
	if !replicated || spec == "" || spec == "inherit" {
		return spec
	}
	if strings.Contains(spec, "{index}") {
		return strings.ReplaceAll(spec, "{index}", strconv.Itoa(idx))
	}
	return spec
}
