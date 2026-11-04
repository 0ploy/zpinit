package machine

import (
	"bytes"
	"strconv"
	"time"

	"github.com/0ploy/zpinit/internal/reaper"
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

// oomVictim returns the pid a kernel log message records the OOM
// killer killing ("... Killed process <pid> (<comm>) ...").
func oomVictim(msg []byte) (int, bool) {
	_, rest, found := bytes.Cut(msg, []byte("Killed process "))
	if !found {
		return 0, false
	}
	digits, _, found := bytes.Cut(rest, []byte(" ("))
	if !found {
		return 0, false
	}
	pid, err := strconv.Atoi(string(digits))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// oomVictims indexes records (one per element) by OOM-killed pid, each
// with the time of its latest kill record.
func oomVictims(records [][]byte) map[int]time.Duration {
	out := map[int]time.Duration{}
	for _, rec := range records {
		ts, msg, ok := parseKmsgRecord(rec)
		if !ok {
			continue
		}
		if pid, ok := oomVictim(msg); ok && ts >= out[pid] {
			out[pid] = ts
		}
	}
	return out
}

// attributeOOM marks in out each candidate the index records as killed
// at or after its Since (minus oomSlack). It reports whether every
// candidate is now marked.
func attributeOOM(victims map[int]time.Duration, cands []reaper.OOMCandidate, out []bool) bool {
	all := true
	for i, c := range cands {
		if !out[i] {
			if ts, ok := victims[c.PID]; ok && ts+oomSlack >= c.Since {
				out[i] = true
			}
		}
		all = all && out[i]
	}
	return all
}

// oomKilledIn reports whether records hold an OOM kill of pid logged
// at or after since (minus oomSlack).
func oomKilledIn(records [][]byte, pid int, since time.Duration) bool {
	out := []bool{false}
	attributeOOM(oomVictims(records), []reaper.OOMCandidate{{PID: pid, Since: since}}, out)
	return out[0]
}
