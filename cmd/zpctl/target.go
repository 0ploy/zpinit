package main

import (
	"fmt"
	"strconv"
	"strings"
)

// translateSupervisorTarget rewrites a supervisord group:process target
// into zpinit's native form so supervisord's `restart name:*` target
// syntax keeps working. A supervisord program maps onto a zpinit
// service name; its processes map onto replicas. Args without a ':'
// pass through unchanged.
//
//	name:*      -> name      (all replicas; the common "restart the group")
//	name:name   -> name      (all replicas; the numprocs=1 default where
//	                          the single process is named after the group)
//	name:name_N -> name/N     (replica N; the default numprocs>1 naming
//	                          %(program_name)s_%(process_num)0Nd)
//
// Any other process suffix is rejected with a clear error rather than
// silently widening to the whole group, so a typo or a customized
// supervisord process_name surfaces instead of restarting everything.
//
// Translation happens client-side, before the request leaves zpctl, so
// the daemon only ever sees native targets and the shim works against
// any zpinit version, including a PID 1 started before this feature
// landed (PID 1 can't be hot-swapped without recreating the container).
func translateSupervisorTarget(arg string) (string, error) {
	group, proc, ok := strings.Cut(arg, ":")
	if !ok {
		return arg, nil
	}
	switch {
	case proc == "*", proc == group:
		return group, nil
	case strings.HasPrefix(proc, group+"_"):
		suffix := strings.TrimPrefix(proc, group+"_")
		idx, err := strconv.Atoi(suffix)
		if err != nil || idx < 0 {
			return "", fmt.Errorf("unrecognized supervisord target %q: expected %s:* or %s:%s_<index>", arg, group, group, group)
		}
		return group + "/" + strconv.Itoa(idx), nil
	default:
		return "", fmt.Errorf("unrecognized supervisord target %q: use %s:* to address every replica", arg, group)
	}
}

// translateTargets rewrites every supervisord group:process target in a
// verb's argument list to its native form. Flags (--wait, --json, -f),
// signal names, and "all" contain no ':' and pass through untouched, so
// applying it across all of a verb's args is safe: only colon-bearing
// targets are rewritten.
func translateTargets(args []string) ([]string, error) {
	out := make([]string, len(args))
	for i, a := range args {
		t, err := translateSupervisorTarget(a)
		if err != nil {
			return nil, err
		}
		out[i] = t
	}
	return out, nil
}
