package resources

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fakeNotifier is a cgroupNotify the test drives by hand. It is
// bubble-safe: everything it does is a channel operation, so synctest
// still sees the watcher's goroutine as durably blocked. rearms counts
// observations, because the loop calls rearm() exactly once per
// observation and nothing else does.
//
// The counter is atomic: it is written by the watcher's loop goroutine
// and read by the test, and synctest.Wait() orders those two but is
// not a happens-before edge the race detector recognises for a plain
// field.
type fakeNotifier struct {
	ch     chan struct{}
	rearms atomic.Int64
}

func (f *fakeNotifier) C() <-chan struct{} { return f.ch }
func (f *fakeNotifier) rearm()             { f.rearms.Add(1) }
func (f *fakeNotifier) close()             {}

// A burst of wakes must collapse into one observation per cooldown
// window, and the burst must still be acted on — deferred, not dropped.
func TestWatcher_InotifyWakesAreRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cg, proc := newTestRoots(t)
		writeCgroupV2(t, cg, 200000, 100000, 1<<30)
		writeProc(t, proc, 8, 16<<30)

		w := newBubbleWatcher(20*time.Millisecond, 20*time.Millisecond)
		// Unbuffered: every send below is provably consumed by the
		// loop, so the test measures the cooldown rather than a
		// channel's buffering.
		fn := &fakeNotifier{ch: make(chan struct{})}
		w.notifier = fn
		w.SetPollInterval(time.Hour) // only wakes can drive an observation

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w.Start(ctx)
		synctest.Wait()

		const burst = 50
		for i := 0; i < burst; i++ {
			fn.ch <- struct{}{}
		}
		synctest.Wait()

		// 1 arm before the select loop + 1 for the first wake (the
		// cooldown window had not opened yet). The other 49 are folded
		// into the single deferred observation that has not fired yet.
		if got := fn.rearms.Load(); got != 2 {
			t.Fatalf("observations during the burst = %d, want 2 (1 initial arm + 1 wake); %d wakes were sent",
				got, burst)
		}

		// Let the window expire: the deferred wake must now be honoured.
		time.Sleep(observeCooldown + 10*time.Millisecond)
		synctest.Wait()
		if got := fn.rearms.Load(); got != 3 {
			t.Fatalf("observations after the cooldown = %d, want 3; the deferred wake was dropped", got)
		}
	})
}

// The cooldown must delay a change, never lose it: a limit that moves
// during a burst still has to reach subscribers.
func TestWatcher_CooldownDefersButDoesNotDropAChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cg, proc := newTestRoots(t)
		writeCgroupV2(t, cg, 200000, 100000, 1<<30) // 2 CPUs
		writeProc(t, proc, 8, 16<<30)

		w := newBubbleWatcher(20*time.Millisecond, 20*time.Millisecond)
		fn := &fakeNotifier{ch: make(chan struct{})}
		w.notifier = fn
		w.SetPollInterval(time.Hour)
		sub, _ := w.Subscribe()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w.Start(ctx)
		synctest.Wait()

		// Burn the cooldown window with a wake that carries no change,
		// so the interesting one lands inside the window.
		fn.ch <- struct{}{}
		synctest.Wait()

		writeCgroupV2(t, cg, 400000, 100000, 1<<30) // 2 -> 4 CPUs
		fn.ch <- struct{}{}                         // deferred by the cooldown

		select {
		case c := <-sub:
			if c.Snapshot.CPUCount != 4 {
				t.Fatalf("CPUCount = %d, want 4", c.Snapshot.CPUCount)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("change made during the cooldown window was never committed")
		}
	})
}
