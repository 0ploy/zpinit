package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// TestBoot_ReloadRemovingUnbootedServiceDoesNotKillBoot pins R2 from
// the review: the control socket and SIGHUP are live during initial
// boot, so a reload can remove a service that boot has not reached
// yet. Boot's snapshot then holds a stale runner whose Run goroutine
// is gone; that must be skipped, not treated as a boot failure that
// exits the whole container.
func TestBoot_ReloadRemovingUnbootedServiceDoesNotKillBoot(t *testing.T) {
	a := dummyService("10_a", true) // readiness probe gates boot progress
	b := dummyService("20_b", false)
	f := newOrchFixture(t, []config.Service{a, b}, "")
	f.cfg.Globals.BootTimeout = config.Duration(500 * time.Millisecond)

	// Gate 10_a's readiness probe so the test controls exactly when
	// boot moves past it.
	release := make(chan struct{})
	f.orch.prober = func(ctx context.Context, _ []string, _ []string, _ string) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	aProc := f.nextProcess(2 * time.Second) // 10_a spawned; boot now parked in its probe

	// While boot is parked, a reload removes 20_b.
	newCfg := &config.Config{
		Services: []config.Service{a},
		Globals:  f.cfg.Globals,
	}
	if _, err := f.orch.Reload(context.Background(), newCfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	close(release) // 10_a becomes ready; boot proceeds to the stale 20_b pointer

	// Boot must complete without failing: Run keeps running (bootDone
	// stays empty) even after 20_b's StartCtx times out and is skipped.
	select {
	case code := <-f.bootDone:
		t.Fatalf("orchestrator exited with %d; boot treated the removed service as a failure", code)
	case <-time.After(1200 * time.Millisecond): // > BootTimeout for the stale runner
	}

	// 20_b must never have spawned.
	select {
	case p := <-f.procs:
		t.Fatalf("unexpected spawn pid %d; removed service was booted", p.PID())
	default:
	}

	// Clean shutdown still works.
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for len(aProc.signalsReceived()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		aProc.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()
	cancel()
	if code := f.awaitExit(5 * time.Second); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}
