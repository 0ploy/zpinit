package supervisor

import (
	"context"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// The exit_code_from watcher decides whether the container follows one
// service down. The distinction it has to draw is WHY that service
// reached a terminal state: a child that ended on its own (or burned
// its crash budget) is the container's job finishing, while an operator
// running `zpctl restart` or `zpctl stop` is just managing a service
// and must never take PID 1 with it. These tests pin both directions,
// plus the property that declining an operator edge does not disarm the
// watcher for the genuine terminal state that may follow.

// watcherFixture is an Orchestrator with exactly one runner, which is
// also the exit_code_from target, and a live watcher installed.
type watcherFixture struct {
	o     *Orchestrator
	r     *Runner
	clock *fakeClock
	procs chan *fakeProcess
}

func newWatcherFixture(t *testing.T, restart config.Restart) *watcherFixture {
	t.Helper()
	svc := config.Service{
		Name: "app", Filename: "10_app.toml", Command: []string{"app"},
		Restart: restart, StopSignal: "TERM",
		StopTimeout:       config.Duration(time.Second),
		BackoffInitial:    config.Duration(time.Second),
		BackoffMax:        config.Duration(30 * time.Second),
		BackoffResetAfter: config.Duration(60 * time.Second),
	}
	clock := newFakeClock(time.Now())
	procs := make(chan *fakeProcess, 16)
	pid := 1000
	r := NewRunner(svc, nil, 0, func(config.Service, []string) (Process, error) {
		pid++
		p := newFakeProcess(pid)
		procs <- p
		return p, nil
	}, clock, testLog())
	// Deterministic backoff timings: see CLAUDE.md on jitterRand.
	r.jitterRand = nil

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	o := &Orchestrator{
		log: testLog(),
		cfg: &config.Config{
			Globals:  config.Globals{ExitCodeFrom: "app", BootTimeout: config.Duration(2 * time.Second)},
			Services: []config.Service{svc},
		},
	}
	o.runners = []*Runner{r}
	o.runnerCtx = ctx
	o.wg = &wg
	o.earlyShutdownCh = make(chan struct{})
	o.spawnRunnerGoroutine(r)
	o.installExitCodeWatcher()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return &watcherFixture{o: o, r: r, clock: clock, procs: procs}
}

// boot brings the service up and returns its process.
func (f *watcherFixture) boot(t *testing.T) *fakeProcess {
	t.Helper()
	if err := f.r.StartCtx(context.Background()); err != nil {
		t.Fatalf("StartCtx: %v", err)
	}
	waitForState(t, f.r, StateRunning)
	return <-f.procs
}

// waitForSignal blocks until p has been signaled, so a test can push
// the child's exit only after the stop has actually been processed.
// Pushing earlier would race: Run would see the buffered exit while the
// runner is still Running and score it as a crash, not a stop.
func waitForSignal(t *testing.T, p *fakeProcess) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(p.signalsReceived()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("process was never signaled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertShutdown(t *testing.T, o *Orchestrator, why string) {
	t.Helper()
	select {
	case <-o.earlyShutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: early shutdown did not fire, want container exit", why)
	}
}

func assertNoShutdown(t *testing.T, o *Orchestrator, why string) {
	t.Helper()
	select {
	case <-o.earlyShutdownCh:
		t.Fatalf("%s: early shutdown fired, want container to stay up", why)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestExitCodeFrom_OperatorRestart is the regression test for the field
// bug: `zpctl restart` on the exit_code_from target transits Stopped on
// its way back up, and the watcher used to read that edge as "the
// service this container exists for has ended".
func TestExitCodeFrom_OperatorRestart(t *testing.T) {
	for _, restart := range []config.Restart{config.RestartAlways, config.RestartNever} {
		t.Run(string(restart), func(t *testing.T) {
			f := newWatcherFixture(t, restart)
			p := f.boot(t)

			done := make(chan error, 1)
			go func() { done <- f.r.RestartCtx(context.Background()) }()
			waitForSignal(t, p)
			p.pushExit(reaper.ExitInfo{Signaled: true, Signal: syscall.SIGTERM})

			if err := <-done; err != nil {
				t.Fatalf("RestartCtx: %v", err)
			}
			waitForState(t, f.r, StateRunning)
			assertNoShutdown(t, f.o, "restart of the exit_code_from target")
		})
	}
}

// TestExitCodeFrom_OperatorStop pins the semantic chosen for a plain
// stop: the operator is managing one service, not ending the container.
// The watcher parks for as long as the target stays stopped.
func TestExitCodeFrom_OperatorStop(t *testing.T) {
	f := newWatcherFixture(t, config.RestartAlways)
	p := f.boot(t)

	if err := f.r.StopCtx(context.Background()); err != nil {
		t.Fatalf("StopCtx: %v", err)
	}
	waitForSignal(t, p)
	p.pushExit(reaper.ExitInfo{Signaled: true, Signal: syscall.SIGTERM})
	waitForState(t, f.r, StateStopped)

	assertNoShutdown(t, f.o, "stop of the exit_code_from target")
}

// TestExitCodeFrom_CleanExit guards the blessed one-off worker pattern:
// `restart = "never"` plus exit_code_from, where the child finishing IS
// the container's job finishing. The operator-stop fix must not touch
// this path.
func TestExitCodeFrom_CleanExit(t *testing.T) {
	f := newWatcherFixture(t, config.RestartNever)
	p := f.boot(t)

	p.pushExit(reaper.ExitInfo{ExitCode: 0})
	waitForState(t, f.r, StateStopped)

	assertShutdown(t, f.o, "clean exit of a restart=never worker")
	if code := f.o.exitCode(); code != 0 {
		t.Errorf("exitCode() = %d, want 0", code)
	}
}

// TestExitCodeFrom_FailedExit is the same path with a nonzero code: the
// whole point of exit_code_from is propagating the worker's failure to
// the orchestrator, so this must still bring the container down.
func TestExitCodeFrom_FailedExit(t *testing.T) {
	f := newWatcherFixture(t, config.RestartNever)
	p := f.boot(t)

	p.pushExit(reaper.ExitInfo{ExitCode: 3})
	waitForState(t, f.r, StateStopped)

	assertShutdown(t, f.o, "failed exit of a restart=never worker")
	if code := f.o.exitCode(); code != 3 {
		t.Errorf("exitCode() = %d, want 3", code)
	}
}

// TestExitCodeFrom_CrashUnderAlways guards behaviour that was already
// correct in the field: under `restart = "always"` a crash goes
// Running -> Backoff and never settles in Stopped, so the container
// rides it out.
func TestExitCodeFrom_CrashUnderAlways(t *testing.T) {
	f := newWatcherFixture(t, config.RestartAlways)
	p := f.boot(t)

	p.pushExit(reaper.ExitInfo{ExitCode: 1})
	waitForState(t, f.r, StateBackoff)

	assertNoShutdown(t, f.o, "single crash under restart=always")
}

// crashToFatal burns the runner's crash budget, leaving it FATAL.
func crashToFatal(t *testing.T, f *watcherFixture, p *fakeProcess) {
	t.Helper()
	for i := 0; i < MaxConsecutiveCrashes; i++ {
		p.pushExit(reaper.ExitInfo{ExitCode: 1})
		if i == MaxConsecutiveCrashes-1 {
			break
		}
		waitForState(t, f.r, StateBackoff)
		f.clock.Advance(31 * time.Second)
		waitForState(t, f.r, StateRunning)
		p = <-f.procs
	}
	waitForState(t, f.r, StateFatal)
}

// TestExitCodeFrom_Fatal confirms what the field report could only
// infer: crash-looping to FATAL under `restart = "always"` does take
// the container down with the child's code. That is the combination the
// report argued for, transient crash retries but permanent failure
// exits, so it needs a test of its own.
func TestExitCodeFrom_Fatal(t *testing.T) {
	f := newWatcherFixture(t, config.RestartAlways)
	p := f.boot(t)

	crashToFatal(t, f, p)

	assertShutdown(t, f.o, "crash budget exhausted under restart=always")
	if code := f.o.exitCode(); code != 1 {
		t.Errorf("exitCode() = %d, want 1", code)
	}
}

// TestExitCodeFrom_RestartThenFatal is the property that forced the
// watcher to be a loop rather than a single WaitTerminal: declining an
// operator restart must not disarm the watcher. If it returned instead
// of parking, a service restarted once could later crash-loop to FATAL
// with nobody left watching, and the container would sit there dead.
func TestExitCodeFrom_RestartThenFatal(t *testing.T) {
	f := newWatcherFixture(t, config.RestartAlways)
	p := f.boot(t)

	done := make(chan error, 1)
	go func() { done <- f.r.RestartCtx(context.Background()) }()
	waitForSignal(t, p)
	p.pushExit(reaper.ExitInfo{Signaled: true, Signal: syscall.SIGTERM})
	if err := <-done; err != nil {
		t.Fatalf("RestartCtx: %v", err)
	}
	waitForState(t, f.r, StateRunning)
	assertNoShutdown(t, f.o, "restart of the exit_code_from target")

	crashToFatal(t, f, <-f.procs)
	assertShutdown(t, f.o, "FATAL after an earlier operator restart")
}

// TestLastTerminal pins the latch the watcher depends on, including the
// property that distinguishes it from StoppedManually: it survives the
// Start that a restart issues moments later.
func TestLastTerminal(t *testing.T) {
	f := newWatcherFixture(t, config.RestartAlways)
	if info := f.r.LastTerminal(); info.Valid {
		t.Errorf("never-terminal runner: LastTerminal().Valid = true, want false")
	}
	p := f.boot(t)

	done := make(chan error, 1)
	go func() { done <- f.r.RestartCtx(context.Background()) }()
	waitForSignal(t, p)
	p.pushExit(reaper.ExitInfo{Signaled: true, Signal: syscall.SIGTERM})
	if err := <-done; err != nil {
		t.Fatalf("RestartCtx: %v", err)
	}
	waitForState(t, f.r, StateRunning)

	info := f.r.LastTerminal()
	if !info.Valid || !info.Manual || info.State != StateStopped {
		t.Errorf("after restart: LastTerminal() = %+v, want {Stopped true true}", info)
	}
	if f.r.StoppedManually() {
		t.Error("StoppedManually() = true after the restart's Start; the latch must not rely on it")
	}
}
