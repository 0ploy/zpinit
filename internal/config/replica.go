package config

import (
	"strconv"
	"strings"
)

// ReplicaLogPath rewrites a [log] path spec for one replica.
//
//	total <= 1                 -> spec verbatim (no rewriting)
//	spec == "" or "inherit"    -> spec verbatim
//	spec contains "{index}"    -> placeholder replaced with idx
//	otherwise                  -> spec verbatim; all replicas share the file
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
func ReplicaLogPath(spec string, idx, total int) string {
	if total <= 1 || spec == "" || spec == "inherit" {
		return spec
	}
	if strings.Contains(spec, "{index}") {
		return strings.ReplaceAll(spec, "{index}", strconv.Itoa(idx))
	}
	return spec
}
