package machine

import (
	"bytes"
	"strconv"
	"time"
)

// OOM detection, machine mode only. When a tracked child dies of
// SIGKILL, the reaper asks whether the kernel's OOM killer sent it.
// The answer comes from the kernel log (/dev/kmsg): the OOM killer
// logs "<reason>: Killed process <pid> (<comm>) ..." for every victim
// ("Out of memory", "Memory cgroup out of memory"). Matching the pid,
// and only on records written after the child was spawned, attributes
// the kill exactly; a global counter (/proc/vmstat oom_kill) could not
// tell which process died.
//
// Never in a container: there /dev/kmsg is, if readable at all, the
// host's log, with pids from another namespace, so a match would be
// wrong rather than merely missing.

// oomSlack absorbs the difference between the kernel log's clock and
// CLOCK_MONOTONIC: both count from boot, but they are not the same
// clock. A pid reused within this window of an OOM kill of its
// previous owner is the only way to misattribute.
const oomSlack = 2 * time.Second

// parseKmsgRecord splits one /dev/kmsg record,
// "<prio>,<seq>,<usec>,<flags>[,...];<message>\n[ KEY=VALUE\n...]",
// into its timestamp (time since boot) and message.
func parseKmsgRecord(rec []byte) (ts time.Duration, msg []byte, ok bool) {
	head, rest, found := bytes.Cut(rec, []byte{';'})
	if !found {
		return 0, nil, false
	}
	fields := bytes.SplitN(head, []byte{','}, 4)
	if len(fields) < 3 {
		return 0, nil, false
	}
	usec, err := strconv.ParseUint(string(fields[2]), 10, 63)
	if err != nil {
		return 0, nil, false
	}
	msg, _, _ = bytes.Cut(rest, []byte{'\n'})
	return time.Duration(usec) * time.Microsecond, msg, true
}

// isOOMKill reports whether a kernel log message records the OOM
// killer killing pid.
func isOOMKill(msg []byte, pid int) bool {
	needle := []byte("Killed process " + strconv.Itoa(pid) + " (")
	return bytes.Contains(msg, needle)
}

// oomKilledIn scans records (one per element) for an OOM kill of pid
// logged at or after since (minus oomSlack).
func oomKilledIn(records [][]byte, pid int, since time.Duration) bool {
	for _, rec := range records {
		ts, msg, ok := parseKmsgRecord(rec)
		if !ok || ts+oomSlack < since {
			continue
		}
		if isOOMKill(msg, pid) {
			return true
		}
	}
	return false
}
