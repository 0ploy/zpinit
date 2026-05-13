package supervisor

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/resources"
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

func TestOnResourceChange_FiltersByDimension(t *testing.T) {
	// Two services: one listens on cpu only, one on memory only.
	// A cpu-dimension change must touch only the first.
	procs := make(chan *fakeProcess, 8)
	var spawns sync.Mutex
	spawnedCmds := map[string]int{}
	spawn := func(svc config.Service, _ []string) (Process, error) {
		spawns.Lock()
		spawnedCmds[svc.Name]++
		spawns.Unlock()
		p := newFakeProcess(1000 + spawnedCmds[svc.Name])
		procs <- p
		return p, nil
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := newFakeClock(time.Unix(0, 0))
	cpuSvc := config.Service{
		Name: "cpuapp", Filename: "10_cpu.toml",
		Command:        []string{"x"},
		Restart:        config.RestartAlways,
		StopSignal:     "TERM",
		StopTimeout:    config.Duration(500 * time.Millisecond),
		ReloadSignal:   "HUP",
		ReloadOnChange: []string{resources.DimCPU},
	}
	memSvc := config.Service{
		Name: "memapp", Filename: "20_mem.toml",
		Command:        []string{"x"},
		Restart:        config.RestartAlways,
		StopSignal:     "TERM",
		StopTimeout:    config.Duration(500 * time.Millisecond),
		ReloadSignal:   "HUP",
		ReloadOnChange: []string{resources.DimMemory},
	}
	o := &Orchestrator{
		log:     log,
		spawner: spawn,
		clock:   clk,
		cfg: &config.Config{
			Services: []config.Service{cpuSvc, memSvc},
			Globals:  config.Globals{ExitCodeFrom: "default"},
		},
	}
	runners := []*Runner{
		NewRunner(cpuSvc, nil, 0, spawn, clk, log),
		NewRunner(memSvc, nil, 0, spawn, clk, log),
	}
	for _, r := range runners {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go r.Run(ctx)
		if err := r.StartCtx(ctx); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	// Drain procs so spawn doesn't block; collect the two so we can
	// inspect signals later.
	cpuProc := <-procs
	memProc := <-procs

	o.mu.Lock()
	o.runners = runners
	o.runnerCtx = context.Background()
	o.mu.Unlock()

	// Wait for both runners to reach Running so SignalGroup has a
	// process to target.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runners[0].State() == StateRunning && runners[1].State() == StateRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	change := resources.Change{
		Snapshot:   resources.Snapshot{CPUQuota: 4.0, CPUCount: 4, MemoryBytes: 1 << 30},
		Dimensions: []string{resources.DimCPU},
	}
	o.OnResourceChange(change)

	// Reload is dispatched in a detached goroutine; allow it to
	// land. SignalGroup is synchronous from the reload-one path, so
	// once it's been called the signal slice is populated.
	if !waitFor(50*time.Millisecond, func() bool { return len(cpuProc.signalsReceived()) > 0 }) {
		t.Errorf("cpuapp should have received HUP after cpu-dim change")
	}
	if len(memProc.signalsReceived()) != 0 {
		t.Errorf("memapp should NOT have received a signal: %v", memProc.signalsReceived())
	}
}

func waitFor(timeout time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return pred()
}

func TestOnResourceChange_UpdatesResourceEnv(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := &Orchestrator{
		log: log,
		cfg: &config.Config{Globals: config.Globals{ExitCodeFrom: "default"}},
	}
	var seen map[string]string
	o.SetBaseEnvBuilder(func(_, rsrc map[string]string) []string {
		seen = rsrc
		return nil
	})

	change := resources.Change{
		Snapshot:   resources.Snapshot{CPUQuota: 2.0, CPUCount: 2, MemoryBytes: 1 << 30},
		Dimensions: []string{resources.DimCPU},
	}
	o.OnResourceChange(change)
	if seen[resources.EnvCPUCount] != "2" {
		t.Errorf("ZPINIT_CPU_COUNT = %q, want 2", seen[resources.EnvCPUCount])
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
