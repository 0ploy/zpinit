package resources

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func newTestRoots(t *testing.T) (cgroup, proc string) {
	t.Helper()
	cgroup = t.TempDir()
	proc = t.TempDir()
	t.Setenv("ZPINIT_CGROUP_ROOT", cgroup)
	t.Setenv("ZPINIT_PROC_ROOT", proc)
	return cgroup, proc
}

func writeCgroupV2(t *testing.T, cgroup string, quota, period int64, mem uint64) {
	t.Helper()
	q := "max"
	if quota > 0 {
		q = itoaInt(int(quota))
	}
	writeFile(t, filepath.Join(cgroup, "cpu.max"), q+" "+itoaInt(int(period))+"\n")
	m := "max"
	if mem > 0 {
		m = itoaInt(int(mem))
	}
	writeFile(t, filepath.Join(cgroup, "memory.max"), m+"\n")
}

func writeProc(t *testing.T, proc string, cpus int, memBytes uint64) {
	t.Helper()
	writeFile(t, filepath.Join(proc, "cpuinfo"), procCPUInfo(cpus))
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       "+itoaInt(int(memBytes/1024))+" kB\n")
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func TestWatcher_NoChangeNoEmit(t *testing.T) {
	cg, proc := newTestRoots(t)
	writeCgroupV2(t, cg, 200000, 100000, 1<<30)
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 20*time.Millisecond, 20*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetPollInterval(10 * time.Millisecond)
	sub, _ := w.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	select {
	case c := <-sub:
		t.Fatalf("unexpected commit: %+v", c)
	case <-time.After(150 * time.Millisecond):
		// no change → no emit, as expected
	}
}

func TestWatcher_ScaleUpCommitsAfterDebounce(t *testing.T) {
	cg, proc := newTestRoots(t)
	writeCgroupV2(t, cg, 200000, 100000, 1<<30) // 2 CPUs, 1 GiB
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 50*time.Millisecond, 1*time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetPollInterval(10 * time.Millisecond)
	sub, _ := w.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Bump CPU to 4 cores.
	writeCgroupV2(t, cg, 400000, 100000, 1<<30)

	select {
	case c := <-sub:
		if c.Snapshot.CPUCount != 4 {
			t.Errorf("CPUCount = %d, want 4", c.Snapshot.CPUCount)
		}
		if len(c.Dimensions) != 1 || c.Dimensions[0] != DimCPU {
			t.Errorf("Dimensions = %v, want [cpu]", c.Dimensions)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no commit observed")
	}
}

func TestWatcher_ScaleDownUsesLongerDebounce(t *testing.T) {
	cg, proc := newTestRoots(t)
	writeCgroupV2(t, cg, 400000, 100000, 1<<30) // 4 CPUs
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 10*time.Millisecond, 200*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetPollInterval(10 * time.Millisecond)
	sub, _ := w.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Drop to 2 CPUs.
	writeCgroupV2(t, cg, 200000, 100000, 1<<30)

	// Should NOT have committed within scale-up window (10 ms).
	select {
	case c := <-sub:
		t.Fatalf("scale-down committed too eagerly: %+v", c)
	case <-time.After(80 * time.Millisecond):
	}
	// Should commit by the scale-down window (200 ms + slack).
	select {
	case c := <-sub:
		if c.Snapshot.CPUCount != 2 {
			t.Errorf("CPUCount = %d, want 2", c.Snapshot.CPUCount)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("scale-down never committed")
	}
}

func TestWatcher_TransientFlipDoesNotEmit(t *testing.T) {
	cg, proc := newTestRoots(t)
	writeCgroupV2(t, cg, 200000, 100000, 1<<30)
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 100*time.Millisecond, 100*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetPollInterval(10 * time.Millisecond)
	sub, _ := w.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Bump to 4, then revert to 2 within the debounce window.
	writeCgroupV2(t, cg, 400000, 100000, 1<<30)
	time.Sleep(40 * time.Millisecond)
	writeCgroupV2(t, cg, 200000, 100000, 1<<30)

	select {
	case c := <-sub:
		t.Fatalf("transient flip should not commit: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWatcher_SubIntegerWobbleDoesNotEmit(t *testing.T) {
	cg, proc := newTestRoots(t)
	// 1.2 CPUs: floor is 1.
	writeCgroupV2(t, cg, 120000, 100000, 1<<30)
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 30*time.Millisecond, 30*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetPollInterval(10 * time.Millisecond)
	sub, _ := w.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Move quota to 1.4 (floor still 1).
	writeCgroupV2(t, cg, 140000, 100000, 1<<30)

	select {
	case c := <-sub:
		t.Fatalf("sub-integer quota wobble should not commit: %+v", c)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestWatcher_MemoryChange(t *testing.T) {
	cg, proc := newTestRoots(t)
	writeCgroupV2(t, cg, 200000, 100000, 1<<30) // 1 GiB
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 30*time.Millisecond, 200*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetPollInterval(10 * time.Millisecond)
	sub, _ := w.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Bump memory to 2 GiB; CPU unchanged.
	writeCgroupV2(t, cg, 200000, 100000, 2<<30)

	select {
	case c := <-sub:
		if c.Snapshot.MemoryBytes != 2<<30 {
			t.Errorf("MemoryBytes = %d, want 2 GiB", c.Snapshot.MemoryBytes)
		}
		if len(c.Dimensions) != 1 || c.Dimensions[0] != DimMemory {
			t.Errorf("Dimensions = %v, want [memory]", c.Dimensions)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("memory change never committed")
	}
}

func TestWatcher_CurrentSeedsAtStart(t *testing.T) {
	cg, proc := newTestRoots(t)
	writeCgroupV2(t, cg, 300000, 100000, 1<<30)
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 10*time.Millisecond, 10*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetPollInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Allow the goroutine a tick to settle but we don't actually
	// need it: Start primes Current synchronously.
	got := w.Current()
	if got.CPUCount != 3 {
		t.Errorf("Current.CPUCount = %d, want 3", got.CPUCount)
	}
}

func TestWatcher_StopCancelsLoop(t *testing.T) {
	cg, proc := newTestRoots(t)
	writeCgroupV2(t, cg, 200000, 100000, 1<<30)
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 10*time.Millisecond, 10*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetPollInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	cancel()
	// We don't have a Done channel; the test ensures cancel is
	// safe + does not hang. A bit of slack so the goroutine
	// observes ctx.Err().
	time.Sleep(30 * time.Millisecond)
}
