//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procStats is the subset of per-process /proc data `zpctl status
// --verbose` surfaces: resident memory, accumulated CPU time, and
// open-fd count. Anything that can't be read (the process died
// between PID capture and stat, /proc not mounted) returns zero
// values without an error: status is informational, not a contract,
// and missing data is preferable to refusing to render the line.
//
// Linux only. The non-linux stub returns zero for everything; macOS
// development never exercises the verbose path.
type procStats struct {
	RSSBytes   uint64
	CPUSeconds float64
	FDCount    int
}

// readProcStats reads /proc/<pid>/statm, /proc/<pid>/stat, and
// counts entries in /proc/<pid>/fd. None of these blocks on
// anything but the kernel's own internal locks; ENOENT (PID gone)
// is normal and silently produces a partial result.
func readProcStats(pid int) procStats {
	if pid <= 0 {
		return procStats{}
	}
	root := "/proc/" + strconv.Itoa(pid)
	var s procStats
	if rss, ok := readRSSBytes(filepath.Join(root, "statm")); ok {
		s.RSSBytes = rss
	}
	if cpu, ok := readCPUSeconds(filepath.Join(root, "stat")); ok {
		s.CPUSeconds = cpu
	}
	if n, ok := countFDs(filepath.Join(root, "fd")); ok {
		s.FDCount = n
	}
	return s
}

// readRSSBytes parses field 2 of /proc/<pid>/statm (resident pages)
// and converts to bytes via the runtime page size. statm format is
// stable across kernels and avoids the field-counting fragility of
// /proc/<pid>/stat for RSS.
func readRSSBytes(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}

// readCPUSeconds parses utime + stime from /proc/<pid>/stat
// (positional fields 14 and 15, in clock ticks). The first field
// is the PID; the second is "(comm)" which can contain spaces and
// parens, so we find the last ')' and start tokenization after it.
// Conversion to seconds uses _SC_CLK_TCK; we hard-code 100 because
// every Linux kernel zpinit can run on uses 100 Hz user-space clock
// ticks (cgo would be the only portable way to query, and we ship
// CGO_ENABLED=0).
func readCPUSeconds(path string) (float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+1 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[rparen+1:])
	// Field 3 of /proc/<pid>/stat is "state"; we're now past PID and
	// comm, so fields[0]=state, fields[1]=ppid, ... fields[11]=utime,
	// fields[12]=stime (1-indexed positions 14 and 15 after the comm
	// group).
	if len(fields) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	const clockTicksPerSecond = 100
	return float64(utime+stime) / clockTicksPerSecond, true
}

// countFDs returns the number of entries in /proc/<pid>/fd. Cheap
// (one syscall per entry) and tolerant: a fd that vanishes between
// readdir and the loop doesn't affect the count we already have.
func countFDs(path string) (int, bool) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, false
	}
	return len(entries), true
}
