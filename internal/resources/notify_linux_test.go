//go:build linux

package resources

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitWake reports whether a wake arrived within d.
func waitWake(n cgroupNotify, d time.Duration) bool {
	select {
	case <-n.C():
		return true
	case <-time.After(d):
		return false
	}
}

func TestInotifyNotifier_WakesOnLimitWrite(t *testing.T) {
	root := t.TempDir()
	cpuMax := filepath.Join(root, "cpu.max")
	writeFile(t, cpuMax, "200000 100000\n")

	n := newCgroupNotifyPlatform(cgroupLimitPaths(legacyLayout(root, true)), discardLog())
	defer n.close()
	n.rearm()

	// Drain anything the arm itself may have produced.
	select {
	case <-n.C():
	default:
	}

	if err := os.WriteFile(cpuMax, []byte("100000 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitWake(n, 2*time.Second) {
		t.Fatal("no wake after writing cpu.max")
	}
}

// Detect() reads these files on every observation, and an observation
// is what a wake triggers. If reads produced events the notifier would
// drive itself in a loop, which is why only IN_MODIFY is requested.
func TestInotifyNotifier_ReadsDoNotWake(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "cpu.max"), "200000 100000\n")
	writeFile(t, filepath.Join(root, "memory.max"), "1073741824\n")
	t.Setenv("ZPINIT_CGROUP_ROOT", root)
	t.Setenv("ZPINIT_PROC_ROOT", t.TempDir())

	n := newCgroupNotifyPlatform(cgroupLimitPaths(legacyLayout(root, true)), discardLog())
	defer n.close()
	n.rearm()
	select {
	case <-n.C():
	default:
	}

	for i := 0; i < 20; i++ {
		_ = Detect()
	}
	if waitWake(n, 300*time.Millisecond) {
		t.Fatal("Detect()'s reads woke the notifier; the event loop would be self-sustaining")
	}
}

// A cgroupfs remount replaces the inode: the kernel drops the watch
// (IN_IGNORED) and writes to the new inode are silent. rearm must
// restore it without any state tracking by the caller.
func TestInotifyNotifier_RearmSelfHealsAfterInodeSwap(t *testing.T) {
	root := t.TempDir()
	cpuMax := filepath.Join(root, "cpu.max")
	writeFile(t, cpuMax, "200000 100000\n")

	n := newCgroupNotifyPlatform(cgroupLimitPaths(legacyLayout(root, true)), discardLog())
	defer n.close()
	n.rearm()

	if err := os.Remove(cpuMax); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cpuMax, []byte("300000 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Drain the DELETE_SELF/IGNORED wakes the removal produced.
	for waitWake(n, 200*time.Millisecond) {
	}

	// Without a rearm the new inode is unwatched; prove that first so
	// the rearm below is testing something real.
	if err := os.WriteFile(cpuMax, []byte("400000 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if waitWake(n, 300*time.Millisecond) {
		t.Fatal("stale watch unexpectedly still delivering; test no longer covers the self-heal")
	}

	n.rearm()
	if err := os.WriteFile(cpuMax, []byte("500000 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitWake(n, 2*time.Second) {
		t.Fatal("rearm did not restore the watch after the inode was replaced")
	}
}

// Arming against a root where nothing exists must be harmless: that is
// the normal case for the other hierarchy version's paths, and the
// whole case on a host with no cgroupfs.
func TestInotifyNotifier_MissingPathsAreHarmless(t *testing.T) {
	n := newCgroupNotifyPlatform(cgroupLimitPaths(legacyLayout(t.TempDir(), true)), discardLog())
	defer n.close()
	n.rearm()
	n.rearm()
	if waitWake(n, 200*time.Millisecond) {
		t.Fatal("unexpected wake with no watchable files")
	}
}

// The point of the whole change: with a backstop far in the future,
// only inotify can drive a commit. If the wake path is broken this
// test times out rather than passing slowly.
func TestWatcher_InotifyDrivesCommitWithoutPolling(t *testing.T) {
	cg, proc := newTestRoots(t)
	writeCgroupV2(t, cg, 200000, 100000, 1<<30) // 2 CPUs
	writeProc(t, proc, 8, 16<<30)

	w := NewWatcher(0, 0, 20*time.Millisecond, 20*time.Millisecond, discardLog())
	// An hour: any commit within the test window came from inotify.
	w.SetPollInterval(time.Hour)
	if w.notifier == nil {
		t.Fatal("expected a platform notifier on linux")
	}
	sub, _ := w.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Give the loop a moment to arm before the write.
	time.Sleep(100 * time.Millisecond)
	writeCgroupV2(t, cg, 400000, 100000, 1<<30) // 2 -> 4 CPUs

	select {
	case c := <-sub:
		if c.Snapshot.CPUCount != 4 {
			t.Fatalf("CPUCount = %d, want 4", c.Snapshot.CPUCount)
		}
		if len(c.Dimensions) != 1 || c.Dimensions[0] != DimCPU {
			t.Fatalf("dimensions = %v, want [cpu]", c.Dimensions)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no commit: inotify did not drive the observation (backstop poll is 1h)")
	}
}
