package resources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withRoots points Detect at tempdir fixtures by setting the
// ZPINIT_CGROUP_ROOT and ZPINIT_PROC_ROOT overrides. t.Setenv handles
// cleanup. Empty string means "leave path unreadable so the source is
// skipped" — use a path inside t.TempDir that we haven't populated.
func withRoots(t *testing.T, cgroup, proc string) {
	t.Helper()
	if cgroup == "" {
		cgroup = filepath.Join(t.TempDir(), "nocgroup")
	}
	if proc == "" {
		proc = filepath.Join(t.TempDir(), "noproc")
	}
	t.Setenv("ZPINIT_CGROUP_ROOT", cgroup)
	t.Setenv("ZPINIT_PROC_ROOT", proc)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetect_CgroupV2_Limited(t *testing.T) {
	cg := t.TempDir()
	proc := t.TempDir()
	// 200% of one CPU, period 100ms.
	writeFile(t, filepath.Join(cg, "cpu.max"), "200000 100000\n")
	writeFile(t, filepath.Join(cg, "memory.max"), "1073741824\n") // 1 GiB
	writeFile(t, filepath.Join(proc, "cpuinfo"), procCPUInfo(8))
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       16777216 kB\n")
	withRoots(t, cg, proc)

	s := Detect()
	if s.CPUQuota != 2.0 {
		t.Errorf("CPUQuota = %v, want 2.0", s.CPUQuota)
	}
	if s.CPUCount != 2 {
		t.Errorf("CPUCount = %d, want 2", s.CPUCount)
	}
	if s.MemoryBytes != 1<<30 {
		t.Errorf("MemoryBytes = %d, want %d", s.MemoryBytes, 1<<30)
	}
}

func TestDetect_CgroupV2_Unlimited_FallsBackToProc(t *testing.T) {
	cg := t.TempDir()
	proc := t.TempDir()
	writeFile(t, filepath.Join(cg, "cpu.max"), "max 100000\n")
	writeFile(t, filepath.Join(cg, "memory.max"), "max\n")
	writeFile(t, filepath.Join(proc, "cpuinfo"), procCPUInfo(4))
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       8388608 kB\n")
	withRoots(t, cg, proc)

	s := Detect()
	if s.CPUCount != 4 {
		t.Errorf("CPUCount = %d, want 4 (cgroup unlimited, /proc says 4)", s.CPUCount)
	}
	if s.MemoryBytes != 8*1024*1024*1024 {
		t.Errorf("MemoryBytes = %d, want 8 GiB", s.MemoryBytes)
	}
}

func TestDetect_CgroupV1(t *testing.T) {
	cg := t.TempDir()
	proc := t.TempDir()
	writeFile(t, filepath.Join(cg, "cpu", "cpu.cfs_quota_us"), "150000\n")
	writeFile(t, filepath.Join(cg, "cpu", "cpu.cfs_period_us"), "100000\n")
	writeFile(t, filepath.Join(cg, "memory", "memory.limit_in_bytes"), "536870912\n") // 512 MiB
	writeFile(t, filepath.Join(proc, "cpuinfo"), procCPUInfo(8))
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       16777216 kB\n")
	withRoots(t, cg, proc)

	s := Detect()
	if s.CPUQuota != 1.5 {
		t.Errorf("CPUQuota = %v, want 1.5", s.CPUQuota)
	}
	if s.CPUCount != 1 {
		t.Errorf("CPUCount = %d, want 1 (floor of 1.5)", s.CPUCount)
	}
	if s.MemoryBytes != 512*1024*1024 {
		t.Errorf("MemoryBytes = %d, want 512 MiB", s.MemoryBytes)
	}
}

func TestDetect_CgroupV1_UnlimitedSentinel(t *testing.T) {
	cg := t.TempDir()
	proc := t.TempDir()
	writeFile(t, filepath.Join(cg, "cpu", "cpu.cfs_quota_us"), "-1\n")
	writeFile(t, filepath.Join(cg, "cpu", "cpu.cfs_period_us"), "100000\n")
	// Near-MaxInt64 sentinel used by cgroup v1 for "unlimited".
	writeFile(t, filepath.Join(cg, "memory", "memory.limit_in_bytes"), "9223372036854771712\n")
	writeFile(t, filepath.Join(proc, "cpuinfo"), procCPUInfo(2))
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       4194304 kB\n")
	withRoots(t, cg, proc)

	s := Detect()
	if s.CPUCount != 2 {
		t.Errorf("CPUCount = %d, want 2 (v1 quota -1 means unlimited)", s.CPUCount)
	}
	if s.MemoryBytes != 4*1024*1024*1024 {
		t.Errorf("MemoryBytes = %d, want 4 GiB (v1 huge sentinel ignored)", s.MemoryBytes)
	}
}

func TestDetect_NoCgroup_ProcOnly(t *testing.T) {
	proc := t.TempDir()
	writeFile(t, filepath.Join(proc, "cpuinfo"), procCPUInfo(3))
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       2097152 kB\n")
	withRoots(t, "", proc)

	s := Detect()
	if s.CPUCount != 3 {
		t.Errorf("CPUCount = %d, want 3", s.CPUCount)
	}
	if s.MemoryBytes != 2*1024*1024*1024 {
		t.Errorf("MemoryBytes = %d, want 2 GiB", s.MemoryBytes)
	}
}

func TestDetect_MinOfSources(t *testing.T) {
	// Container inside a VM: VM has 8 vCPUs, cgroup limits to 2.
	cg := t.TempDir()
	proc := t.TempDir()
	writeFile(t, filepath.Join(cg, "cpu.max"), "200000 100000\n")
	writeFile(t, filepath.Join(cg, "memory.max"), "1073741824\n")
	writeFile(t, filepath.Join(proc, "cpuinfo"), procCPUInfo(8))
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       16777216 kB\n")
	withRoots(t, cg, proc)

	s := Detect()
	if s.CPUCount != 2 {
		t.Errorf("CPUCount = %d, want 2 (cgroup is more restrictive)", s.CPUCount)
	}
	if s.MemoryBytes != 1<<30 {
		t.Errorf("MemoryBytes = %d, want 1 GiB (cgroup is more restrictive)", s.MemoryBytes)
	}
}

func TestWithReserves(t *testing.T) {
	s := Snapshot{CPUQuota: 4.0, CPUCount: 4, MemoryBytes: 4 * 1024 * 1024 * 1024}
	r := s.WithReserves(0.5, 256*1024*1024)
	if r.CPUQuota != 3.5 {
		t.Errorf("CPUQuota = %v, want 3.5", r.CPUQuota)
	}
	if r.CPUCount != 3 {
		t.Errorf("CPUCount = %d, want 3", r.CPUCount)
	}
	if r.MemoryBytes != 4*1024*1024*1024-256*1024*1024 {
		t.Errorf("MemoryBytes wrong: %d", r.MemoryBytes)
	}
}

func TestWithReserves_FloorAtOne(t *testing.T) {
	s := Snapshot{CPUQuota: 0.5, CPUCount: 1}
	r := s.WithReserves(1.0, 0)
	if r.CPUCount != 1 {
		t.Errorf("CPUCount = %d, want 1 (floor enforced)", r.CPUCount)
	}
}

func TestEnvVars_Formatting(t *testing.T) {
	cases := []struct {
		quota float64
		want  string
	}{
		{1.0, "1"},
		{1.5, "1.5"},
		{2.25, "2.25"},
		{0.0, "0"},
	}
	for _, c := range cases {
		s := Snapshot{CPUQuota: c.quota, CPUCount: cpuFloor(c.quota), MemoryBytes: 0}
		got := s.EnvVars()[EnvCPUQuota]
		if got != c.want {
			t.Errorf("quota %v: got %q, want %q", c.quota, got, c.want)
		}
	}
}

func TestEnvVars_KeysComplete(t *testing.T) {
	s := Snapshot{CPUQuota: 2.0, CPUCount: 2, MemoryBytes: 1024}
	env := s.EnvVars()
	for _, k := range []string{EnvCPUCount, EnvCPUQuota, EnvMemoryBytes} {
		if _, ok := env[k]; !ok {
			t.Errorf("missing env key %q", k)
		}
	}
	if env[EnvCPUCount] != "2" {
		t.Errorf("CPU count: %q", env[EnvCPUCount])
	}
	if env[EnvMemoryBytes] != "1024" {
		t.Errorf("memory: %q", env[EnvMemoryBytes])
	}
}

// procCPUInfo synthesizes a /proc/cpuinfo with n processor lines.
func procCPUInfo(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("processor\t: ")
		b.WriteString(itoa(i))
		b.WriteString("\nvendor_id\t: TestVendor\n\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
