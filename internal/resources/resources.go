// Package resources detects the container's effective CPU and memory
// budget from cgroupfs (v2 unified, v1 legacy) and /proc, and exposes
// the result to children via env variables.
//
// In a container, cgroup limits are authoritative. In a microVM (or
// bare metal), the kernel only sees the allocated vCPUs / memory, so
// /proc is authoritative. Both can be true at once (a container inside
// a VM), so we take the min of every source we can read.
//
// Test hooks: ZPINIT_CGROUP_ROOT and ZPINIT_PROC_ROOT override the
// canonical /sys/fs/cgroup and /proc paths. Production always reads
// the canonical paths.
package resources

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Env variable names zpinit injects into every child's environment.
// Operator [env] tables (globals + per-service) may not set these;
// validation rejects them so the detected values are never silently
// shadowed.
const (
	EnvCPUCount    = "ZPINIT_CPU_COUNT"
	EnvCPUQuota    = "ZPINIT_CPU_QUOTA"
	EnvMemoryBytes = "ZPINIT_MEMORY_BYTES"
)

// Snapshot is the resource budget detected at one moment in time.
// CPUQuota is the fractional CPU count (e.g. 1.5 for "150% of one
// core"); CPUCount is its integer floor, clamped at a minimum of 1.
// MemoryBytes is the byte budget; 0 means "unlimited or unknown" —
// children that read the env var will see "0" and can treat that as
// "no limit applied."
type Snapshot struct {
	CPUQuota    float64
	CPUCount    int
	MemoryBytes uint64
}

// Detect reads cgroupfs and /proc and returns the most restrictive
// view: min of every source that reported a finite value. Sources
// that report "unlimited" or fail to parse are skipped, not treated
// as zero.
func Detect() Snapshot {
	cgroupRoot := os.Getenv("ZPINIT_CGROUP_ROOT")
	if cgroupRoot == "" {
		cgroupRoot = "/sys/fs/cgroup"
	}
	procRoot := os.Getenv("ZPINIT_PROC_ROOT")
	if procRoot == "" {
		procRoot = "/proc"
	}

	procCPU := procCPUCount(procRoot)
	procMem := procMemoryBytes(procRoot)
	cgCPU, cgMem := readCgroup(cgroupRoot)

	cpu := float64(procCPU)
	if cgCPU > 0 && cgCPU < cpu {
		cpu = cgCPU
	}
	if cpu <= 0 {
		// /proc was unreadable and cgroup said unlimited; fall back
		// to Go's view so we always report something usable.
		cpu = float64(runtime.NumCPU())
	}

	mem := procMem
	if cgMem > 0 && (mem == 0 || cgMem < mem) {
		mem = cgMem
	}

	return Snapshot{
		CPUQuota:    cpu,
		CPUCount:    cpuFloor(cpu),
		MemoryBytes: mem,
	}
}

// WithReserves returns a snapshot reduced by the operator-configured
// reservations. Reserves below zero are clamped; CPU below 1 floor is
// also clamped (we always advertise at least one CPU to children).
func (s Snapshot) WithReserves(reserveCPU float64, reserveMemory uint64) Snapshot {
	cpu := s.CPUQuota - reserveCPU
	if cpu < 0 {
		cpu = 0
	}
	var mem uint64
	if s.MemoryBytes > reserveMemory {
		mem = s.MemoryBytes - reserveMemory
	}
	return Snapshot{
		CPUQuota:    cpu,
		CPUCount:    cpuFloor(cpu),
		MemoryBytes: mem,
	}
}

// EnvVars renders the snapshot as the three canonical env keys that
// zpinit injects into every child's environment.
func (s Snapshot) EnvVars() map[string]string {
	return map[string]string{
		EnvCPUCount:    strconv.Itoa(s.CPUCount),
		EnvCPUQuota:    formatQuota(s.CPUQuota),
		EnvMemoryBytes: strconv.FormatUint(s.MemoryBytes, 10),
	}
}

func cpuFloor(q float64) int {
	n := int(q)
	if n < 1 {
		n = 1
	}
	return n
}

func formatQuota(q float64) string {
	if q <= 0 {
		return "0"
	}
	s := strconv.FormatFloat(q, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}

func readCgroup(root string) (float64, uint64) {
	if v2cpu, v2mem, ok := readCgroupV2(root); ok {
		return v2cpu, v2mem
	}
	return readCgroupV1(root)
}

// readCgroupV2 reads the unified hierarchy. ok=true if either file
// existed (we treat the v2 root as present and authoritative for
// either dimension once we see one of its files), so the caller
// doesn't fall through to v1 on a mixed system.
func readCgroupV2(root string) (float64, uint64, bool) {
	cpu, cpuFound := readV2CPU(filepath.Join(root, "cpu.max"))
	mem, memFound := readV2Memory(filepath.Join(root, "memory.max"))
	if !cpuFound && !memFound {
		return 0, 0, false
	}
	return cpu, mem, true
}

func readV2CPU(path string) (float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, true
	}
	if fields[0] == "max" {
		return 0, true
	}
	quota, err1 := strconv.ParseInt(fields[0], 10, 64)
	period, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil || period <= 0 || quota <= 0 {
		return 0, true
	}
	return float64(quota) / float64(period), true
}

func readV2Memory(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0, true
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, true
	}
	return n, true
}

func readCgroupV1(root string) (float64, uint64) {
	quota := readInt64(filepath.Join(root, "cpu", "cpu.cfs_quota_us"))
	period := readInt64(filepath.Join(root, "cpu", "cpu.cfs_period_us"))
	var cpu float64
	if quota > 0 && period > 0 {
		cpu = float64(quota) / float64(period)
	}
	memRaw := readUint64(filepath.Join(root, "memory", "memory.limit_in_bytes"))
	var mem uint64
	// v1 uses a near-MaxInt64 sentinel for "unlimited". Anything
	// above 2^62 is treated as no limit; real container limits are
	// orders of magnitude below that.
	if memRaw > 0 && memRaw < (uint64(1)<<62) {
		mem = memRaw
	}
	return cpu, mem
}

// procCPUCount counts the "processor" lines in /proc/cpuinfo. Scanned
// line-by-line rather than ReadFile + strings.Split: this runs on
// every watcher poll (default 1s) for the daemon's whole life, and on
// a high-core host the split would allocate a large []string (plus the
// whole-file string) each tick. The scanner reuses one buffer.
func procCPUCount(procRoot string) int {
	f, err := os.Open(filepath.Join(procRoot, "cpuinfo"))
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "processor") && strings.Contains(line, ":") {
			n++
		}
	}
	return n
}

// procMemoryBytes reads MemTotal from /proc/meminfo. Line-by-line scan
// for the same per-poll allocation reason as procCPUCount.
func procMemoryBytes(procRoot string) uint64 {
	f, err := os.Open(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		// /proc/meminfo MemTotal is reported in kB.
		return n * 1024
	}
	return 0
}

func readInt64(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return n
}

func readUint64(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return n
}
