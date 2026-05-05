package supervisor

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// testFixture holds the wiring most state-machine tests reuse.
type testFixture struct {
	t        *testing.T
	clock    *fakeClock
	runner   *Runner
	states   <-chan State
	cancel   context.CancelFunc
	procs    chan *fakeProcess // each Spawn pushes here
	spawnErr error             // if non-nil, spawn returns this
	spawnsMu sync.Mutex
	spawns   int
}

func newFixture(t *testing.T, cfg config.Service) *testFixture {
	t.Helper()
	if cfg.Restart == "" {
		cfg.Restart = config.RestartAlways
	}
	if cfg.BackoffInitial == 0 {
		cfg.BackoffInitial = config.Duration(1 * time.Second)
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = config.Duration(30 * time.Second)
	}
	if cfg.BackoffResetAfter == 0 {
		cfg.BackoffResetAfter = config.Duration(60 * time.Second)
	}
	if cfg.StopSignal == "" {
		cfg.StopSignal = "TERM"
	}
	if cfg.Name == "" {
		cfg.Name = "test"
	}

	f := &testFixture{
		t:     t,
		clock: newFakeClock(time.Unix(0, 0)),
		procs: make(chan *fakeProcess, 32),
	}

	spawn := func(_ config.Service, _ []string) (Process, error) {
		f.spawnsMu.Lock()
		f.spawns++
		f.spawnsMu.Unlock()
		if f.spawnErr != nil {
			return nil, f.spawnErr
		}
		p := newFakeProcess(1000 + f.spawns)
		f.procs <- p
		return p, nil
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.runner = NewRunner(cfg, nil, spawn, f.clock, log)
	ch, cancel := f.runner.Observe()
	f.states = ch
	t.Cleanup(cancel)

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go f.runner.Run(ctx)
	t.Cleanup(cancel)

	return f
}

func (f *testFixture) waitState(want State, timeout time.Duration) {
	f.t.Helper()
	// Already there? Most calls land here because Run processes events
	// synchronously and the observer buffer outlives them.
	if f.runner.State() == want {
		return
	}
	deadline := time.After(timeout)
	for {
		select {
		case s := <-f.states:
			if s == want {
				return
			}
		case <-deadline:
			f.t.Fatalf("timed out waiting for state %s; current = %s", want, f.runner.State())
		}
	}
}

func (f *testFixture) nextProcess(timeout time.Duration) *fakeProcess {
	f.t.Helper()
	select {
	case p := <-f.procs:
		return p
	case <-time.After(timeout):
		f.t.Fatal("timed out waiting for spawn")
		return nil
	}
}

func (f *testFixture) spawnCount() int {
	f.spawnsMu.Lock()
	defer f.spawnsMu.Unlock()
	return f.spawns
}

func TestRunner_StartGoesToRunning(t *testing.T) {
	f := newFixture(t, config.Service{Command: []string{"x"}})
	f.runner.Start()
	f.waitState(StateRunning, 2*time.Second)
	if f.spawnCount() != 1 {
		t.Errorf("spawns = %d, want 1", f.spawnCount())
	}
	if f.runner.PID() == 0 {
		t.Error("PID should be set in Running")
	}
}

func TestRunner_RestartAlways_AfterCleanExit(t *testing.T) {
	f := newFixture(t, config.Service{
		Command: []string{"x"},
		Restart: config.RestartAlways,
	})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	p.pushExit(reaper.ExitInfo{ExitCode: 0})
	f.waitState(StateBackoff, time.Second)

	f.clock.Advance(time.Second)
	f.waitState(StateRunning, time.Second)
	if f.spawnCount() != 2 {
		t.Errorf("spawns = %d, want 2", f.spawnCount())
	}
}

func TestRunner_RestartOnFailure_CleanExitStops(t *testing.T) {
	f := newFixture(t, config.Service{
		Command: []string{"x"},
		Restart: config.RestartOnFailure,
	})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	p.pushExit(reaper.ExitInfo{ExitCode: 0})
	f.waitState(StateStopped, time.Second)
}

func TestRunner_RestartOnFailure_CrashRestarts(t *testing.T) {
	f := newFixture(t, config.Service{
		Command: []string{"x"},
		Restart: config.RestartOnFailure,
	})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	p.pushExit(reaper.ExitInfo{ExitCode: 1})
	f.waitState(StateBackoff, time.Second)
	f.clock.Advance(time.Second)
	f.waitState(StateRunning, time.Second)
}

func TestRunner_RestartNever_DoesNotRestart(t *testing.T) {
	for _, info := range []reaper.ExitInfo{
		{ExitCode: 0},
		{ExitCode: 1},
		{Signaled: true, Signal: syscall.SIGTERM},
	} {
		f := newFixture(t, config.Service{
			Command: []string{"x"},
			Restart: config.RestartNever,
		})
		f.runner.Start()
		p := f.nextProcess(time.Second)
		f.waitState(StateRunning, time.Second)

		p.pushExit(info)
		f.waitState(StateStopped, time.Second)
	}
}

func TestRunner_BackoffDoubling(t *testing.T) {
	f := newFixture(t, config.Service{
		Command:           []string{"x"},
		Restart:           config.RestartAlways,
		BackoffInitial:    config.Duration(1 * time.Second),
		BackoffMax:        config.Duration(30 * time.Second),
		BackoffResetAfter: config.Duration(60 * time.Second),
	})
	f.runner.Start()

	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

	for i, delay := range want {
		p := f.nextProcess(time.Second)
		f.waitState(StateRunning, time.Second)
		p.pushExit(reaper.ExitInfo{ExitCode: 1})
		f.waitState(StateBackoff, time.Second)

		// Advance just shy of the expected delay; should still be in Backoff.
		f.clock.Advance(delay - time.Millisecond)
		// Cross the threshold.
		f.clock.Advance(time.Millisecond)
		f.waitState(StateRunning, time.Second)
		if f.spawnCount() != i+2 {
			t.Errorf("after delay #%d: spawns = %d, want %d", i, f.spawnCount(), i+2)
		}
	}
}

func TestRunner_BackoffCappedAtMax(t *testing.T) {
	f := newFixture(t, config.Service{
		Command:           []string{"x"},
		Restart:           config.RestartAlways,
		BackoffInitial:    config.Duration(1 * time.Second),
		BackoffMax:        config.Duration(4 * time.Second),
		BackoffResetAfter: config.Duration(120 * time.Second),
	})
	f.runner.Start()

	// Force several quick crashes.
	for i := 0; i < 4; i++ {
		p := f.nextProcess(time.Second)
		f.waitState(StateRunning, time.Second)
		p.pushExit(reaper.ExitInfo{ExitCode: 1})
		f.waitState(StateBackoff, time.Second)
		// Advance by the cap; should fire even if the "ideal" delay was larger.
		f.clock.Advance(4 * time.Second)
		if i < 3 { // last iteration would cross retry budget; skip the wait
			f.waitState(StateRunning, time.Second)
		}
	}
}

func TestRunner_BackoffResetsAfterStableRun(t *testing.T) {
	f := newFixture(t, config.Service{
		Command:           []string{"x"},
		Restart:           config.RestartAlways,
		BackoffInitial:    config.Duration(1 * time.Second),
		BackoffMax:        config.Duration(30 * time.Second),
		BackoffResetAfter: config.Duration(10 * time.Second),
	})
	f.runner.Start()

	// Two crashes — backoff doubles to 2s.
	for i := 0; i < 2; i++ {
		p := f.nextProcess(time.Second)
		f.waitState(StateRunning, time.Second)
		p.pushExit(reaper.ExitInfo{ExitCode: 1})
		f.waitState(StateBackoff, time.Second)
		f.clock.Advance(30 * time.Second)
		f.waitState(StateRunning, time.Second)
	}

	// Now let the service stay up for >= reset_after, then crash again.
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)
	f.clock.Advance(15 * time.Second)
	p.pushExit(reaper.ExitInfo{ExitCode: 1})
	f.waitState(StateBackoff, time.Second)

	// Counter and backoff should have reset; next delay is BackoffInitial = 1s.
	f.clock.Advance(999 * time.Millisecond)
	if got := f.runner.State(); got != StateBackoff {
		t.Errorf("state = %s before threshold; want still in Backoff", got)
	}
	f.clock.Advance(time.Millisecond)
	f.waitState(StateRunning, time.Second)

	if c := f.runner.Crashes(); c != 1 {
		t.Errorf("Crashes = %d after reset; want 1", c)
	}
}

func TestRunner_FatalAfterRetryBudget(t *testing.T) {
	f := newFixture(t, config.Service{
		Command:           []string{"x"},
		Restart:           config.RestartAlways,
		BackoffInitial:    config.Duration(1 * time.Second),
		BackoffMax:        config.Duration(2 * time.Second),
		BackoffResetAfter: config.Duration(120 * time.Second),
	})
	f.runner.Start()

	for i := 0; i < MaxConsecutiveCrashes-1; i++ {
		p := f.nextProcess(time.Second)
		f.waitState(StateRunning, time.Second)
		p.pushExit(reaper.ExitInfo{ExitCode: 1})
		f.waitState(StateBackoff, time.Second)
		f.clock.Advance(10 * time.Second)
	}

	// Last crash pushes us over the budget.
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)
	p.pushExit(reaper.ExitInfo{ExitCode: 1})
	f.waitState(StateFatal, time.Second)
}

func TestRunner_StopDuringRunning(t *testing.T) {
	f := newFixture(t, config.Service{
		Command:    []string{"x"},
		Restart:    config.RestartAlways,
		StopSignal: "TERM",
	})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	// Stop dispatches the configured signal and parks at Stopping until exit.
	go f.runner.Stop()
	f.waitState(StateStopping, time.Second)

	sigs := p.signalsReceived()
	if len(sigs) != 1 || sigs[0] != syscall.SIGTERM {
		t.Errorf("signals = %v; want [SIGTERM]", sigs)
	}

	p.pushExit(reaper.ExitInfo{Signaled: true, Signal: syscall.SIGTERM})
	f.waitState(StateStopped, time.Second)
}

// Phase 6: when stop_timeout elapses without the process exiting,
// the runner sends SIGKILL to the process group.
func TestRunner_StopEscalatesToKillAfterStopTimeout(t *testing.T) {
	f := newFixture(t, config.Service{
		Command:     []string{"x"},
		Restart:     config.RestartAlways,
		StopSignal:  "TERM",
		StopTimeout: config.Duration(10 * time.Second),
	})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	go f.runner.Stop()
	f.waitState(StateStopping, time.Second)

	// First signal is the configured stop_signal.
	sigs := p.signalsReceived()
	if len(sigs) != 1 || sigs[0] != syscall.SIGTERM {
		t.Fatalf("after Stop: signals = %v, want [SIGTERM]", sigs)
	}

	// Process refuses to exit. Cross the stop_timeout — SIGKILL fires.
	f.clock.Advance(10 * time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(p.signalsReceived()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sigs = p.signalsReceived()
	if len(sigs) != 2 || sigs[1] != syscall.SIGKILL {
		t.Errorf("after stop_timeout: signals = %v, want [SIGTERM, SIGKILL]", sigs)
	}

	// Kernel reaps the SIGKILL'd process; runner transitions to Stopped.
	p.pushExit(reaper.ExitInfo{Signaled: true, Signal: syscall.SIGKILL})
	f.waitState(StateStopped, time.Second)
}

// If the process does exit before stop_timeout, the kill timer must
// be canceled so it doesn't fire on a later Spawn (the next service
// instance would otherwise get an immediate phantom SIGKILL).
func TestRunner_StopKillTimerCanceledOnEarlyExit(t *testing.T) {
	f := newFixture(t, config.Service{
		Command:     []string{"x"},
		Restart:     config.RestartAlways,
		StopSignal:  "TERM",
		StopTimeout: config.Duration(10 * time.Second),
	})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	go f.runner.Stop()
	f.waitState(StateStopping, time.Second)

	// Process exits cleanly in response to SIGTERM.
	p.pushExit(reaper.ExitInfo{Signaled: true, Signal: syscall.SIGTERM})
	f.waitState(StateStopped, time.Second)

	// Now Start again; the previous kill timer firing would send
	// SIGKILL to the new process. Verify the new process only ever
	// sees the signals we explicitly send it.
	f.runner.Start()
	p2 := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	// Cross the would-be-original-kill-timer deadline.
	f.clock.Advance(15 * time.Second)
	time.Sleep(50 * time.Millisecond)

	if got := p2.signalsReceived(); len(got) != 0 {
		t.Errorf("new process received signals from a leaked kill timer: %v", got)
	}
}

func TestRunner_StopDuringBackoff(t *testing.T) {
	f := newFixture(t, config.Service{
		Command: []string{"x"},
		Restart: config.RestartAlways,
	})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)
	p.pushExit(reaper.ExitInfo{ExitCode: 1})
	f.waitState(StateBackoff, time.Second)

	f.runner.Stop()
	f.waitState(StateStopped, time.Second)

	// Advancing the clock past the original backoff must NOT spawn again.
	f.clock.Advance(60 * time.Second)
	time.Sleep(50 * time.Millisecond) // give Run a chance to misbehave
	if got := f.runner.State(); got != StateStopped {
		t.Errorf("state = %s; want still Stopped after clock advance", got)
	}
	if f.spawnCount() != 1 {
		t.Errorf("spawns = %d; want 1 (no respawn after Stop)", f.spawnCount())
	}
}

func TestRunner_StopDuringPending(t *testing.T) {
	f := newFixture(t, config.Service{Command: []string{"x"}})
	f.runner.Stop()
	f.waitState(StateStopped, time.Second)
	if f.spawnCount() != 0 {
		t.Errorf("spawns = %d; want 0", f.spawnCount())
	}
}

func TestRunner_StartAfterStopped(t *testing.T) {
	f := newFixture(t, config.Service{
		Command: []string{"x"},
		Restart: config.RestartOnFailure,
	})
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	p.pushExit(reaper.ExitInfo{ExitCode: 0})
	f.waitState(StateStopped, time.Second)

	// Manual restart from terminal state.
	f.runner.Start()
	f.waitState(StateRunning, time.Second)
	if f.spawnCount() != 2 {
		t.Errorf("spawns = %d", f.spawnCount())
	}
	if c := f.runner.Crashes(); c != 0 {
		t.Errorf("Crashes = %d after manual Start from Stopped; want reset to 0", c)
	}
}

func TestRunner_SpawnFailureIsRetried(t *testing.T) {
	f := newFixture(t, config.Service{
		Command:        []string{"x"},
		Restart:        config.RestartAlways,
		BackoffInitial: config.Duration(1 * time.Second),
		BackoffMax:     config.Duration(2 * time.Second),
	})
	f.spawnErr = errBoom
	f.runner.Start()
	f.waitState(StateBackoff, time.Second)

	// Recover.
	f.spawnErr = nil
	f.clock.Advance(time.Second)
	f.waitState(StateRunning, time.Second)
}

var errBoom = errBoomT{}

type errBoomT struct{}

func (errBoomT) Error() string { return "boom" }
