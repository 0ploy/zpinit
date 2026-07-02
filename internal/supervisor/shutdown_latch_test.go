package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
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
	err := o.registerAndBoot([]reloadBootJob{{runner: r}}, nil, nil)
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
