package supervisor

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// reloadOne fans out one of three behaviors (signal, command,
// fallback restart). Each branch deserves its own deterministic
// test against the fake spawner/process surface.

func TestReloadOne_Signal(t *testing.T) {
	cfg := config.Service{
		Name:         "nginx",
		Command:      []string{"nginx"},
		ReloadSignal: "HUP",
		StopSignal:   "TERM",
		StopTimeout:  config.Duration(time.Second),
	}
	f := newFixture(t, cfg)
	f.runner.Start()
	p := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	orch := &Orchestrator{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := orch.reloadOne(context.Background(), f.runner); err != nil {
		t.Fatalf("reloadOne: %v", err)
	}

	// SignalGroup is invoked synchronously by reloadOne, so the
	// signal must already be observable on the fake process.
	got := p.signalsReceived()
	if len(got) != 1 || got[0] != syscall.SIGHUP {
		t.Errorf("signals = %v, want [SIGHUP]", got)
	}
	if f.runner.State() != StateRunning {
		t.Errorf("state = %s, want Running (signal-reload does not stop)", f.runner.State())
	}
}

func TestReloadOne_Command(t *testing.T) {
	cfg := config.Service{
		Name:          "nginx",
		Command:       []string{"nginx"},
		ReloadCommand: []string{"/usr/sbin/nginx", "-s", "reload"},
		StopSignal:    "TERM",
		StopTimeout:   config.Duration(time.Second),
	}
	f := newFixture(t, cfg)
	f.runner.Start()
	_ = f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	// Capture invocation and synthesize a clean exit on the channel.
	var gotCmd []string
	exitCh := make(chan reaper.ExitInfo, 1)
	exitCh <- reaper.ExitInfo{PID: 9999, ExitCode: 0}
	orch := &Orchestrator{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		oneShot: func(name string, command, env []string) (<-chan reaper.ExitInfo, error) {
			gotCmd = command
			return exitCh, nil
		},
	}
	if err := orch.reloadOne(context.Background(), f.runner); err != nil {
		t.Fatalf("reloadOne: %v", err)
	}
	if !reflect.DeepEqual(gotCmd, cfg.ReloadCommand) {
		t.Errorf("one-shot command = %v, want %v", gotCmd, cfg.ReloadCommand)
	}
}

func TestReloadOne_CommandNonZeroExitIsNotAnError(t *testing.T) {
	cfg := config.Service{
		Name:          "x",
		Command:       []string{"x"},
		ReloadCommand: []string{"/bin/false"},
		StopSignal:    "TERM",
		StopTimeout:   config.Duration(time.Second),
	}
	f := newFixture(t, cfg)
	f.runner.Start()
	_ = f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	exitCh := make(chan reaper.ExitInfo, 1)
	exitCh <- reaper.ExitInfo{PID: 1, ExitCode: 17}
	orch := &Orchestrator{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		oneShot: func(_ string, _, _ []string) (<-chan reaper.ExitInfo, error) {
			return exitCh, nil
		},
	}
	if err := orch.reloadOne(context.Background(), f.runner); err != nil {
		t.Errorf("non-zero exit should be logged but not returned: %v", err)
	}
}

func TestReloadOne_CommandTimeout(t *testing.T) {
	cfg := config.Service{
		Name:          "x",
		Command:       []string{"x"},
		ReloadCommand: []string{"/bin/true"},
		StopSignal:    "TERM",
		StopTimeout:   config.Duration(time.Second),
	}
	f := newFixture(t, cfg)
	f.runner.Start()
	_ = f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	// Never-firing channel.
	exitCh := make(chan reaper.ExitInfo)
	orch := &Orchestrator{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		oneShot: func(_ string, _, _ []string) (<-chan reaper.ExitInfo, error) {
			return exitCh, nil
		},
	}
	// Short ctx so the test doesn't actually wait reloadCommandTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := orch.reloadOne(ctx, f.runner)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestReloadOne_FallbackFullRestart(t *testing.T) {
	cfg := config.Service{
		Name:        "worker",
		Command:     []string{"worker"},
		StopSignal:  "TERM",
		StopTimeout: config.Duration(500 * time.Millisecond),
	}
	f := newFixture(t, cfg)
	f.runner.Start()
	p1 := f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	orch := &Orchestrator{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// reloadOne issues StopCtx, which the fake doesn't auto-exit from.
	// Simulate the kernel reaping the child once stop is signaled, so
	// reloadByRestart's WaitTerminal can return.
	done := make(chan error, 1)
	go func() {
		done <- orch.reloadOne(context.Background(), f.runner)
	}()
	// Wait until the runner asks the process to die, then synthesize
	// the exit.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(p1.signalsReceived()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(p1.signalsReceived()) == 0 {
		t.Fatal("StopCtx never delivered the stop signal")
	}
	p1.pushExit(reaper.ExitInfo{ExitCode: 0})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reloadOne (fallback): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reloadOne did not return")
	}
	// Spawn count: 1 for the initial Start, 1 for the fallback restart.
	if f.spawnCount() != 2 {
		t.Errorf("spawns = %d, want 2 (initial + reload-restart)", f.spawnCount())
	}
}

func TestReloadOne_BadSignalNameDefensive(t *testing.T) {
	// Validation rejects this at config-load, but the dispatch
	// helper should not crash if it ever slips through.
	cfg := config.Service{
		Name:         "x",
		Command:      []string{"x"},
		ReloadSignal: "NOTASIGNAL",
		StopSignal:   "TERM",
		StopTimeout:  config.Duration(time.Second),
	}
	f := newFixture(t, cfg)
	f.runner.Start()
	_ = f.nextProcess(time.Second)
	f.waitState(StateRunning, time.Second)

	orch := &Orchestrator{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := orch.reloadOne(context.Background(), f.runner)
	if err == nil {
		t.Fatal("expected error for invalid reload_signal")
	}
}
