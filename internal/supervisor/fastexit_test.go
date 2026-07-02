package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

func waitForState(t *testing.T, r *Runner, want State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.State() != want {
		if time.Now().After(deadline) {
			t.Fatalf("runner state = %s, want %s", r.State(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCompletedCleanly drives a restart=never runner through a real
// spawn → clean-exit lifecycle and pins the discriminator the boot
// paths rely on: clean fast exits count as completion; crashes,
// manual stops, and never-spawned runners do not.
func TestCompletedCleanly(t *testing.T) {
	newNeverRunner := func(spawn Spawner) *Runner {
		return NewRunner(config.Service{
			Name: "job", Filename: "10_job.toml", Command: []string{"x"},
			Restart: config.RestartNever, StopSignal: "TERM",
			StopTimeout: config.Duration(time.Second),
		}, nil, 0, spawn, newFakeClock(time.Now()), testLog())
	}

	t.Run("clean exit counts", func(t *testing.T) {
		procs := make(chan *fakeProcess, 1)
		r := newNeverRunner(func(config.Service, []string) (Process, error) {
			p := newFakeProcess(4711)
			procs <- p
			return p, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go r.Run(ctx)
		if err := r.StartCtx(ctx); err != nil {
			t.Fatal(err)
		}
		(<-procs).pushExit(reaper.ExitInfo{ExitCode: 0})
		waitForState(t, r, StateStopped)
		if !r.CompletedCleanly() {
			t.Error("clean exit 0 under restart=never: CompletedCleanly() = false, want true")
		}
	})

	t.Run("crash does not count", func(t *testing.T) {
		procs := make(chan *fakeProcess, 1)
		r := newNeverRunner(func(config.Service, []string) (Process, error) {
			p := newFakeProcess(4712)
			procs <- p
			return p, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go r.Run(ctx)
		if err := r.StartCtx(ctx); err != nil {
			t.Fatal(err)
		}
		(<-procs).pushExit(reaper.ExitInfo{ExitCode: 3})
		waitForState(t, r, StateStopped)
		if r.CompletedCleanly() {
			t.Error("exit 3: CompletedCleanly() = true, want false")
		}
	})

	t.Run("manual stop does not count", func(t *testing.T) {
		procs := make(chan *fakeProcess, 1)
		r := newNeverRunner(func(config.Service, []string) (Process, error) {
			p := newFakeProcess(4713)
			procs <- p
			return p, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go r.Run(ctx)
		if err := r.StartCtx(ctx); err != nil {
			t.Fatal(err)
		}
		p := <-procs
		if err := r.StopCtx(ctx); err != nil {
			t.Fatal(err)
		}
		// Deliver the "graceful stop worked" exit the kernel would send.
		p.pushExit(reaper.ExitInfo{ExitCode: 0})
		waitForState(t, r, StateStopped)
		if r.CompletedCleanly() {
			t.Error("manual stop: CompletedCleanly() = true, want false")
		}
	})

	t.Run("never spawned does not count", func(t *testing.T) {
		r := newNeverRunner(nil)
		// Force the Pending → Stopped shape a reload removal produces.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go r.Run(ctx)
		if err := r.StopCtx(ctx); err != nil {
			t.Fatal(err)
		}
		waitForState(t, r, StateStopped)
		if r.CompletedCleanly() {
			t.Error("never spawned: CompletedCleanly() = true, want false")
		}
	})
}

// TestBoot_FastCleanExitOneOffSucceeds is the acceptance test for the
// CLAUDE.md-blessed one-off pattern: a `restart = "never"` worker that
// finishes (exit 0) before boot's WaitBootResult looks must not fail
// container startup. The fake spawner returns a process that has
// already exited, making the fast-exit window as wide as possible.
func TestBoot_FastCleanExitOneOffSucceeds(t *testing.T) {
	job := config.Service{
		Name: "migrate", Filename: "10_migrate.toml", Command: []string{"x", "migrate"},
		Restart: config.RestartNever, StopSignal: "TERM",
		StopTimeout: config.Duration(time.Second),
	}
	f := newOrchFixture(t, []config.Service{job, dummyService("20_app", false)}, "migrate")
	// Replace the fixture spawner for the one-off: exit 0 delivered
	// before the Runner even sees the process.
	baseSpawn := f.orch.spawner
	f.orch.spawner = func(svc config.Service, env []string) (Process, error) {
		if svc.Name == "migrate" {
			p := newFakeProcess(9001)
			p.pushExit(reaper.ExitInfo{ExitCode: 0})
			return p, nil
		}
		return baseSpawn(svc, env)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	// 20_app must still boot (proving boot did not abort on the
	// completed one-off), and the supervisor must then shut down via
	// exit_code_from with the worker's exit code 0. Deliver the app's
	// exit once stopAll signals it, like the other exit_code_from
	// tests do.
	app := f.nextProcess(2 * time.Second)
	go func() {
		deadline := time.Now().Add(4 * time.Second)
		for len(app.signalsReceived()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		app.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()
	code := f.awaitExit(5 * time.Second)
	if code != 0 {
		t.Errorf("supervisor exit code = %d, want 0 (worker completed cleanly)", code)
	}
}
