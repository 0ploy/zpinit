package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// --- D1: removal latch -------------------------------------------------

func TestRunner_StartRefusedWhileRemoving(t *testing.T) {
	f := newFixture(t, config.Service{Command: []string{"x"}})
	f.runner.markRemoving()
	if err := f.runner.StartCtx(context.Background()); !errors.Is(err, errBeingRemoved) {
		t.Fatalf("StartCtx during removal: err = %v, want errBeingRemoved", err)
	}
	if f.spawnCount() != 0 {
		t.Fatalf("spawns = %d, want 0 (start must not spawn during removal)", f.spawnCount())
	}
	// A failed removal clears the latch; the runner must be operable
	// again so the next reload retry (or operator start) works.
	f.runner.clearRemoving()
	if err := f.runner.StartCtx(context.Background()); err != nil {
		t.Fatalf("StartCtx after clearRemoving: %v", err)
	}
	f.waitState(StateRunning, 2*time.Second)
}

func TestRemoveServiceGroup_RefusesConcurrentStart(t *testing.T) {
	// The exact D1 interleave: a `zpctl start` racing removeServiceGroup
	// lands while the removal is stopping the service. Without the
	// latch the queued start can respawn the child between WaitTerminal
	// success and deregistration, leaving PID 1 a live unmanaged
	// process.
	f := newFixture(t, config.Service{Command: []string{"x"}})
	f.runner.Start()
	p := f.nextProcess(2 * time.Second)
	f.waitState(StateRunning, 2*time.Second)

	o := &Orchestrator{log: testLog(), cfg: minimalCfg()}
	o.runners = []*Runner{f.runner}

	startErr := make(chan error, 1)
	go func() {
		// Wait for proof that removal is in flight (the stop signal
		// reached the child), then race a start against it.
		deadline := time.Now().Add(2 * time.Second)
		for len(p.signalsReceived()) == 0 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		startErr <- f.runner.StartCtx(context.Background())
		// Let the removal's WaitTerminal complete.
		p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()

	for _, err := range o.removeServiceGroup(context.Background(), []*Runner{f.runner}) {
		if err != nil {
			t.Fatalf("removeServiceGroup: %v", err)
		}
	}
	select {
	case err := <-startErr:
		if !errors.Is(err, errBeingRemoved) {
			t.Fatalf("concurrent StartCtx: err = %v, want errBeingRemoved", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent StartCtx never returned")
	}
	if n := len(o.snapshotRunners()); n != 0 {
		t.Fatalf("runners after removal = %d, want 0", n)
	}
	if f.spawnCount() != 1 {
		t.Fatalf("spawns = %d, want 1 (racing start must not respawn)", f.spawnCount())
	}
	if pid := f.runner.PID(); pid != 0 {
		t.Fatalf("PID after removal = %d, want 0 (no live unmanaged child)", pid)
	}
}

// --- D9: removed queued boot jobs must not stall the pipeline ----------

func TestRunReloadBoots_SkipsRemovedJob(t *testing.T) {
	spawned := make(chan string, 8)
	spawn := func(svc config.Service, _ []string) (Process, error) {
		p := newFakeProcess(1000 + len(svc.Name))
		spawned <- svc.Name
		return p, nil
	}
	o := &Orchestrator{
		log: testLog(),
		cfg: &config.Config{Globals: config.Globals{
			// Deliberately long: if the removed job is NOT skipped, its
			// StartCtx parks for this long while holding reloadBootMu
			// and the assertion below trips on the 2s bound.
			BootTimeout:  config.Duration(30 * time.Second),
			ExitCodeFrom: "default",
		}},
		spawner: spawn,
		clock:   newFakeClock(time.Now()),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	o.mu.Lock()
	o.runnerCtx = ctx
	o.wg = &wg
	o.mu.Unlock()

	rA := NewRunner(mustService("a", "10_a.toml", nil), nil, 0, spawn, o.clock, o.log)
	rB := NewRunner(mustService("b", "20_b.toml", nil), nil, 0, spawn, o.clock, o.log)
	jobs := []reloadBootJob{{cfg: rA.Cfg(), runner: rA}, {cfg: rB.Cfg(), runner: rB}}

	// Hold reloadBootMu so the detached runReloadBoots parks before
	// job A; that is the window in which a follow-up reload removes A.
	o.reloadBootMu.Lock()
	if err := o.registerAndBoot(jobs, nil, nil); err != nil {
		o.reloadBootMu.Unlock()
		t.Fatalf("registerAndBoot: %v", err)
	}
	// Simulate the removal: stop A's Run goroutine and deregister it,
	// exactly what removeServiceGroup does after a successful stop.
	rA.cancelRun()
	o.mu.Lock()
	keep := o.runners[:0]
	for _, r := range o.runners {
		if r != rA {
			keep = append(keep, r)
		}
	}
	o.runners = keep
	o.mu.Unlock()
	o.reloadBootMu.Unlock()

	// B must boot promptly; a stalled pipeline would sit in A's
	// StartCtx for the full 30s boot_timeout first.
	select {
	case name := <-spawned:
		if name != "b" {
			t.Fatalf("spawned %q, want b (a was removed before boot)", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("b did not boot; removed job stalled the reload-boot pipeline")
	}
	cancel()
	wg.Wait()
}

// --- D8: reload teardown order ------------------------------------------

func TestReload_RemovalsReverseFilenameOrder(t *testing.T) {
	initial := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", nil),
	}
	f, captured := newCapturingFixture(t, initial, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 2, 2*time.Second)

	// Serial boot order means captured[0] is a, captured[1] is b.
	procs := captured.snapshot()
	var orderMu sync.Mutex
	var order []string
	for i, name := range []string{"a", "b"} {
		p := procs[i]
		name := name
		go func() {
			deadline := time.Now().Add(3 * time.Second)
			for len(p.signalsReceived()) == 0 && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			orderMu.Lock()
			order = append(order, name)
			orderMu.Unlock()
			p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
		}()
	}

	newCfg := &config.Config{Services: nil, Globals: f.cfg.Globals}
	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatal(err)
	}

	orderMu.Lock()
	got := append([]string(nil), order...)
	orderMu.Unlock()
	if len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("teardown order = %v, want [b a] (reverse filename order)", got)
	}
	cancel()
	f.awaitExit(3 * time.Second)
}

// --- D4: scoped update after a file rename ------------------------------

func TestReloadScoped_RenameStopsOldCopy(t *testing.T) {
	initial := []config.Service{mustService("web", "10_web.toml", nil)}
	f, captured := newCapturingFixture(t, initial, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 1, 2*time.Second)

	// The old copy exits once the scoped update signals it.
	oldProc := captured.snapshot()[0]
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for len(oldProc.signalsReceived()) == 0 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		oldProc.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()

	// Same service name, renamed file: 10_web.toml -> 20_web.toml.
	newCfg := &config.Config{
		Services: []config.Service{mustService("web", "20_web.toml", nil)},
		Globals:  f.cfg.Globals,
	}
	if _, err := f.orch.ReloadScoped(ctx, newCfg, []string{"web"}); err != nil {
		t.Fatal(err)
	}

	// Exactly one copy must survive, and it must be the renamed file.
	// Before the fix the scoped resolution only picked the disk
	// filename, leaving the old copy running: two services, one name.
	waitForSpawnCount(t, captured, 2, 2*time.Second)
	snap := f.orch.snapshotRunners()
	if len(snap) != 1 {
		names := make([]string, 0, len(snap))
		for _, r := range snap {
			names = append(names, r.Cfg().Filename)
		}
		t.Fatalf("runners after scoped rename update = %v, want just 20_web.toml", names)
	}
	if fn := snap[0].Cfg().Filename; fn != "20_web.toml" {
		t.Fatalf("surviving filename = %s, want 20_web.toml", fn)
	}

	cancel()
	// Only the renamed copy is still alive; the old one already
	// exited above (pushExit closes the channel, so no double push).
	go captured.snapshot()[1].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	f.awaitExit(3 * time.Second)
}

// --- D5: runtime duplicate-name guard ------------------------------------

func TestDiff_DuplicateNameConflictDropsAdd(t *testing.T) {
	// Running service "web" from 10_web.toml whose file is now broken
	// (skipped, kept running). A new file 20_other.toml claims the
	// same name; load-time validation can't see the collision because
	// the broken file's service is absent from the survivor set.
	old := []config.Service{mustService("web", "10_web.toml", nil)}
	o := diffFixture(t, old)
	newCfg := &config.Config{
		Services:     []config.Service{mustService("web", "20_other.toml", nil)},
		SkippedFiles: []config.FileError{{File: "10_web.toml", Err: errors.New("boom")}},
	}
	d := o.computeDiff(newCfg)
	if len(d.add) != 0 {
		t.Fatalf("add = %+v, want empty (name collides with retained runner)", d.add)
	}
	if len(d.remove) != 0 {
		t.Fatalf("remove = %+v, want empty (skipped file keeps its service)", d.remove)
	}
	if len(d.conflicts) != 1 || !strings.Contains(d.conflicts[0], `"web"`) {
		t.Fatalf("conflicts = %v, want one entry naming the duplicate", d.conflicts)
	}
}

func TestDiff_DuplicateNameConflictDropsRestart(t *testing.T) {
	// A restart whose NEW spec renames the service onto a retained
	// runner's name is dropped too; the old spec keeps running.
	old := []config.Service{
		mustService("web", "10_web.toml", nil),
		mustService("worker", "20_worker.toml", nil),
	}
	o := diffFixture(t, old)
	renamed := mustService("web", "20_worker.toml", func(s *config.Service) {
		s.Command = []string{"x", "renamed"}
	})
	newCfg := &config.Config{
		Services: []config.Service{
			renamed,
		},
		SkippedFiles: []config.FileError{{File: "10_web.toml", Err: errors.New("boom")}},
	}
	d := o.computeDiff(newCfg)
	if len(d.restart) != 0 {
		t.Fatalf("restart = %+v, want empty (new name collides with retained runner)", d.restart)
	}
	if len(d.conflicts) != 1 {
		t.Fatalf("conflicts = %v, want one entry", d.conflicts)
	}
}
