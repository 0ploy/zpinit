package supervisor

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/resources"
)

func minimalCfg() *config.Config {
	return &config.Config{Globals: config.Globals{ExitCodeFrom: "default"}}
}

// TestShutdownLatch_RefusesMutations pins the lifecycle latch: once
// stopAll has begun, every path that could register or start a
// runner must refuse. A runner registered after stopAll's snapshot
// is never stopped gracefully; it would die by Pdeathsig SIGKILL
// when PID 1 exits.
func TestShutdownLatch_RefusesMutations(t *testing.T) {
	o := &Orchestrator{log: testLog(), cfg: minimalCfg()}
	o.stopAll() // no runners: only effect is latching o.stopping

	if _, err := o.Reload(context.Background(), minimalCfg()); !errors.Is(err, errShuttingDown) {
		t.Errorf("Reload during shutdown: err = %v, want errShuttingDown", err)
	}
	if _, err := o.ReloadScoped(context.Background(), minimalCfg(), []string{"x"}); !errors.Is(err, errShuttingDown) {
		t.Errorf("ReloadScoped during shutdown: err = %v, want errShuttingDown", err)
	}

	r := NewRunner(config.Service{Name: "late", Filename: "10_late.toml", Command: []string{"true"}},
		nil, 0, nil, newFakeClock(time.Now()), testLog())
	err := o.registerAndBoot([]*Runner{r}, nil, nil)
	if !errors.Is(err, errShuttingDown) {
		t.Errorf("registerAndBoot during shutdown: err = %v, want errShuttingDown", err)
	}
	if n := len(o.snapshotRunners()); n != 0 {
		t.Errorf("registerAndBoot during shutdown registered %d runners, want 0", n)
	}

	// The control verbs that bring services up are refused too.
	s := &ControlServer{orch: o, log: testLog()}
	for _, action := range []string{"start", "restart"} {
		resp := s.cmdStartStopRestart(context.Background(), []string{"all"}, action)
		if resp.Code == 0 {
			t.Errorf("control %s during shutdown: Code = 0, want non-zero", action)
		}
	}

	// OnResourceChange must be a silent no-op (no panic, no scaling).
	o.OnResourceChange(resources.Change{})
}

// TestStopAll_StopsPendingRunner pins that stopAll no longer skips
// Pending runners: a queued start (cmds is buffered) could otherwise
// spawn one after stopAll passed it by, and FIFO ordering of the
// queued stop is what tears that child down again.
func TestStopAll_StopsPendingRunner(t *testing.T) {
	r := NewRunner(config.Service{Name: "p", Filename: "10_p.toml", Command: []string{"true"},
		StopSignal: "TERM"}, nil, 0, nil, newFakeClock(time.Now()), testLog())
	o := &Orchestrator{log: testLog(), cfg: minimalCfg()}
	o.runners = []*Runner{r}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	o.stopAll()

	if got := r.State(); got != StateStopped {
		t.Errorf("pending runner state after stopAll = %s, want %s", got, StateStopped)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner goroutine did not exit")
	}
}

// A restart whose stop half is in flight when stopAll begins must not
// respawn the child: stopAll finds the runner already Stopping, waits
// for the same Stopped edge the restart waits for, and moves on. The
// restart's start half then has to be refused.
func TestStopAll_RefusesRestartInFlight(t *testing.T) {
	f := newFixture(t, config.Service{Name: "app", Filename: "10_app.toml",
		StopTimeout: config.Duration(10 * time.Second)})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	o := &Orchestrator{log: testLog(), cfg: minimalCfg()}
	o.runners = []*Runner{f.runner}

	restartErr := make(chan error, 1)
	go func() { restartErr <- f.runner.RestartCtx(context.Background()) }()
	waitSignals(t, p, 1, time.Second) // the restart's stop half is running

	stopped := make(chan struct{})
	go func() { o.stopAll(); close(stopped) }()
	deadline := time.Now().Add(time.Second)
	// stopAll latches o.stopping and the runners in one critical
	// section, so once this flips the runner is latched too.
	for !o.isStopping() {
		if time.Now().After(deadline) {
			t.Fatal("stopAll did not latch")
		}
		time.Sleep(time.Millisecond)
	}

	p.pushExit(reaper.ExitInfo{Signaled: true, Signal: syscall.SIGTERM})

	select {
	case err := <-restartErr:
		if !errors.Is(err, errShuttingDown) {
			t.Errorf("restart during shutdown: err = %v, want errShuttingDown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart did not return")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stopAll did not return")
	}
	if n := f.spawnCount(); n != 1 {
		t.Errorf("spawns = %d, want 1 (no respawn during shutdown)", n)
	}
	if got := f.runner.State(); got != StateStopped {
		t.Errorf("state = %s, want %s", got, StateStopped)
	}
}
