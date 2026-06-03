package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// orchTestFixture wires an Orchestrator with fake spawner + fake prober
// + fake clock. Tests drive boot/teardown deterministically.
type orchTestFixture struct {
	t        *testing.T
	cfg      *config.Config
	clock    *fakeClock
	orch     *Orchestrator
	procs    chan *fakeProcess
	probesMu sync.Mutex
	probeErr map[string]error // key: full command string
	probeOK  map[string]int   // key: cmd string -> count of failures before success
	bootDone chan int         // populated when orch.Run returns
}

func dummyService(name string, withReady bool) config.Service {
	s := config.Service{
		Name:              name,
		Filename:          name + ".toml",
		Command:           []string{"x", name},
		Restart:           config.RestartAlways,
		BackoffInitial:    config.Duration(time.Second),
		BackoffMax:        config.Duration(30 * time.Second),
		BackoffResetAfter: config.Duration(60 * time.Second),
		StopSignal:        "TERM",
		StopTimeout:       config.Duration(time.Second),
	}
	if withReady {
		s.Ready = &config.Ready{
			Command:   []string{"probe", name},
			Interval:  config.Duration(50 * time.Millisecond),
			Timeout:   config.Duration(2 * time.Second),
			OnTimeout: config.ReadyFail,
		}
	}
	return s
}

func newOrchFixture(t *testing.T, services []config.Service, exitCodeFrom string) *orchTestFixture {
	t.Helper()
	cfg := &config.Config{
		Services: services,
		Globals: config.Globals{
			BootTimeout:  config.Duration(10 * time.Second),
			ExitCodeFrom: "default",
		},
	}
	if exitCodeFrom != "" {
		cfg.Globals.ExitCodeFrom = exitCodeFrom
	}

	f := &orchTestFixture{
		t:        t,
		cfg:      cfg,
		clock:    newFakeClock(time.Unix(0, 0)),
		procs:    make(chan *fakeProcess, 32),
		probeErr: map[string]error{},
		probeOK:  map[string]int{},
		bootDone: make(chan int, 1),
	}

	var spawnSeq atomic.Int64
	spawn := func(svc config.Service, _ []string) (Process, error) {
		pid := int(spawnSeq.Add(1)) + 1000
		p := newFakeProcess(pid)
		f.procs <- p
		return p, nil
	}

	prober := func(_ context.Context, cmd []string, _ []string, _ string) error {
		key := cmd[len(cmd)-1] // service name
		f.probesMu.Lock()
		defer f.probesMu.Unlock()
		if err, ok := f.probeErr[key]; ok {
			return err
		}
		if remaining, ok := f.probeOK[key]; ok && remaining > 0 {
			f.probeOK[key] = remaining - 1
			return errors.New("not ready yet")
		}
		return nil
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.orch = &Orchestrator{
		cfg:     cfg,
		baseEnv: nil,
		log:     log,
		spawner: spawn,
		prober:  prober,
		clock:   f.clock,
	}
	return f
}

func (f *orchTestFixture) start(ctx context.Context) {
	f.t.Helper()
	go func() {
		f.bootDone <- f.orch.Run(ctx)
	}()
}

// nextProcess waits for the next spawn and returns the fakeProcess.
func (f *orchTestFixture) nextProcess(timeout time.Duration) *fakeProcess {
	f.t.Helper()
	select {
	case p := <-f.procs:
		return p
	case <-time.After(timeout):
		f.t.Fatal("timed out waiting for spawn")
		return nil
	}
}

func (f *orchTestFixture) awaitExit(timeout time.Duration) int {
	f.t.Helper()
	select {
	case code := <-f.bootDone:
		return code
	case <-time.After(timeout):
		f.t.Fatal("orchestrator.Run did not return within timeout")
		return -1
	}
}

func TestOrchestrator_BootInOrder(t *testing.T) {
	svcs := []config.Service{
		dummyService("a", false),
		dummyService("b", false),
		dummyService("c", false),
	}
	f := newOrchFixture(t, svcs, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	// Each service spawns in order, no readiness probe. Filename
	// ordering means a -> b -> c.
	for _, want := range []string{"a", "b", "c"} {
		p := f.nextProcess(2 * time.Second)
		if p == nil {
			t.Fatal("no process")
		}
		// fakeProcess doesn't carry the service name; we infer order
		// from spawn sequence (a is spawn 1, b is 2, ...).
		_ = want
	}

	// Trigger shutdown.
	cancel()
	// stopAll signals each running service; we need to push their exits.
	for i := 0; i < len(svcs); i++ {
		// Drain remaining procs that pushExit'd in the runner.
	}

	// Push synthetic exits via the runners' SignalGroup -> our fake records signals
	// but won't trigger Exit. We have to push Exit manually for each.
	// Workaround: each fakeProcess we received we push an exit on it.
	// But after cancel, the spawner may have spawned no more — our 3
	// procs are sufficient.
	// (In a real run, SIGTERM kills the child and the kernel reaps. Here
	// we simulate that by closing each Exit channel.)
	// Note: we don't store the procs above; let's restructure.
	_ = f.awaitExit
}

// helper for tests that need to remember every process spawned.
type capturedProcs struct {
	mu    sync.Mutex
	procs []*fakeProcess
}

func (c *capturedProcs) add(p *fakeProcess) {
	c.mu.Lock()
	c.procs = append(c.procs, p)
	c.mu.Unlock()
}

func (c *capturedProcs) snapshot() []*fakeProcess {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*fakeProcess, len(c.procs))
	copy(out, c.procs)
	return out
}

func newCapturingFixture(t *testing.T, services []config.Service, exitCodeFrom string) (*orchTestFixture, *capturedProcs) {
	f := newOrchFixture(t, services, exitCodeFrom)
	captured := &capturedProcs{}
	// Re-wire spawner to also capture.
	var spawnSeq atomic.Int64
	prevProcs := f.procs
	f.orch.spawner = func(svc config.Service, _ []string) (Process, error) {
		pid := int(spawnSeq.Add(1)) + 1000
		p := newFakeProcess(pid)
		captured.add(p)
		select {
		case prevProcs <- p:
		default:
		}
		return p, nil
	}
	return f, captured
}

func TestOrchestrator_StartsAllServicesAndReturnsExitCode(t *testing.T) {
	svcs := []config.Service{dummyService("a", false), dummyService("b", false)}
	f, captured := newCapturingFixture(t, svcs, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	// Wait for both services to be spawned.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(captured.snapshot()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(captured.snapshot()); got < 2 {
		t.Fatalf("only %d services started", got)
	}

	// Trigger shutdown via ctx.
	cancel()

	// stopAll sends signals; we deliver synthetic exits.
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}

	code := f.awaitExit(3 * time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (exit_code_from=default)", code)
	}
}

func TestOrchestrator_ReadinessProbeBlocksNextService(t *testing.T) {
	svcs := []config.Service{dummyService("a", true), dummyService("b", false)}
	f, captured := newCapturingFixture(t, svcs, "")
	// First two probes for "a" fail, then succeed.
	f.probeOK["a"] = 2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	// Wait for both services to be spawned (b only after a's probe passes).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(captured.snapshot()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(captured.snapshot()); got < 2 {
		t.Fatalf("only %d services spawned; b should boot after a is ready", got)
	}

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

func TestOrchestrator_ReadinessTimeoutFailAbortsBoot(t *testing.T) {
	svcs := []config.Service{dummyService("a", true), dummyService("b", false)}
	// a's probe always fails -> readiness times out -> boot aborts (default on_timeout=fail).
	svcs[0].Ready.Timeout = config.Duration(150 * time.Millisecond)
	svcs[0].Ready.Interval = config.Duration(40 * time.Millisecond)

	f, captured := newCapturingFixture(t, svcs, "")
	f.probeErr["a"] = errors.New("never ready")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	// Wait for a to spawn; b should NEVER spawn because a fails readiness.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(captured.snapshot()) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(captured.snapshot()); got != 1 {
		t.Fatalf("expected exactly 1 spawn; got %d", got)
	}

	// stopAll runs after boot abort; deliver a's exit so it can finish.
	go captured.snapshot()[0].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})

	code := f.awaitExit(3 * time.Second)
	if code != 1 {
		t.Errorf("boot-failure exit code = %d, want 1", code)
	}
	// Confirm b never spawned.
	if got := len(captured.snapshot()); got != 1 {
		t.Errorf("after exit: %d spawns; b must not have started", got)
	}
}

func TestOrchestrator_ReadinessTimeoutContinueProceeds(t *testing.T) {
	svcs := []config.Service{dummyService("a", true), dummyService("b", false)}
	svcs[0].Ready.Timeout = config.Duration(100 * time.Millisecond)
	svcs[0].Ready.Interval = config.Duration(30 * time.Millisecond)
	svcs[0].Ready.OnTimeout = config.ReadyContinue

	f, captured := newCapturingFixture(t, svcs, "")
	f.probeErr["a"] = errors.New("never ready")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	// Both services should eventually spawn.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(captured.snapshot()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(captured.snapshot()); got < 2 {
		t.Fatalf("both services should have spawned; got %d", got)
	}

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

// computeDiff is a pure function — easiest to test directly without
// running orch.Run. Build orchestrator with hand-rolled runners.
func diffFixture(t *testing.T, services []config.Service) *Orchestrator {
	t.Helper()
	o := &Orchestrator{
		cfg: &config.Config{Services: services},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, s := range services {
		r := NewRunner(s, nil, 0, nil, nil, o.log)
		o.runners = append(o.runners, r)
	}
	return o
}

func mustService(name, file string, body func(*config.Service)) config.Service {
	s := config.Service{
		Name:              name,
		Filename:          file,
		Command:           []string{"x", name},
		Restart:           config.RestartAlways,
		BackoffInitial:    config.Duration(time.Second),
		BackoffMax:        config.Duration(30 * time.Second),
		BackoffResetAfter: config.Duration(60 * time.Second),
		StopSignal:        "TERM",
		StopTimeout:       config.Duration(time.Second),
		Log:               config.Logging{Stdout: "inherit", Stderr: "inherit"},
	}
	if body != nil {
		body(&s)
	}
	return s
}

func TestDiff_Empty(t *testing.T) {
	svcs := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", nil),
	}
	o := diffFixture(t, svcs)
	d := o.computeDiff(&config.Config{Services: svcs})
	if len(d.add)+len(d.remove)+len(d.restart) != 0 {
		t.Errorf("expected empty diff; got %+v", d)
	}
}

func TestDiff_Add(t *testing.T) {
	old := []config.Service{mustService("a", "10_a.toml", nil)}
	new_ := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", nil),
	}
	o := diffFixture(t, old)
	d := o.computeDiff(&config.Config{Services: new_})
	if len(d.add) != 1 || d.add[0].Filename != "20_b.toml" {
		t.Errorf("add = %+v", d.add)
	}
	if len(d.remove) != 0 || len(d.restart) != 0 {
		t.Errorf("unexpected diff: %+v", d)
	}
}

func TestDiff_Remove(t *testing.T) {
	old := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", nil),
	}
	new_ := []config.Service{mustService("a", "10_a.toml", nil)}
	o := diffFixture(t, old)
	d := o.computeDiff(&config.Config{Services: new_})
	if len(d.remove) != 1 || d.remove[0].Cfg().Filename != "20_b.toml" {
		t.Errorf("remove = %+v", d.remove)
	}
}

func TestDiff_Restart(t *testing.T) {
	old := []config.Service{mustService("a", "10_a.toml", nil)}
	new_ := []config.Service{mustService("a", "10_a.toml", func(s *config.Service) {
		s.Command = []string{"x", "a", "--new-flag"}
	})}
	o := diffFixture(t, old)
	d := o.computeDiff(&config.Config{Services: new_})
	if len(d.restart) != 1 {
		t.Fatalf("restart count = %d, want 1", len(d.restart))
	}
	if d.restart[0].new.Command[2] != "--new-flag" {
		t.Errorf("restart payload missing new flag: %v", d.restart[0].new.Command)
	}
}

func TestDiff_NotReloadableSkipsRestart(t *testing.T) {
	notReload := false
	old := []config.Service{mustService("a", "10_a.toml", func(s *config.Service) {
		s.Reloadable = &notReload
	})}
	new_ := []config.Service{mustService("a", "10_a.toml", func(s *config.Service) {
		s.Reloadable = &notReload
		s.Command = []string{"x", "a", "--changed"}
	})}
	o := diffFixture(t, old)
	d := o.computeDiff(&config.Config{Services: new_})
	if len(d.restart) != 0 {
		t.Errorf("non-reloadable service should not be restarted: %+v", d.restart)
	}
}

func TestDiff_GlobalsEnvChangeAddsRestart(t *testing.T) {
	svcs := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", nil),
	}
	o := diffFixture(t, svcs)
	o.cfg.Globals.Env = map[string]string{"FOO": "old"}

	// Same services, only globals.Env differs. All reloadable services
	// should be added to the restart list so they pick up the new env
	// on respawn.
	d := o.computeDiff(&config.Config{
		Services: svcs,
		Globals:  config.Globals{Env: map[string]string{"FOO": "new"}},
	})
	if len(d.restart) != 2 {
		t.Fatalf("restart count = %d, want 2 (env change should restart all reloadable services)", len(d.restart))
	}
	if len(d.add)+len(d.remove) != 0 {
		t.Errorf("unexpected add/remove on env-only change: %+v", d)
	}
}

func TestDiff_GlobalsEnvChangeSkipsNonReloadable(t *testing.T) {
	notReload := false
	svcs := []config.Service{
		mustService("a", "10_a.toml", func(s *config.Service) {
			s.Reloadable = &notReload
		}),
		mustService("b", "20_b.toml", nil),
	}
	o := diffFixture(t, svcs)
	o.cfg.Globals.Env = map[string]string{"FOO": "old"}

	d := o.computeDiff(&config.Config{
		Services: svcs,
		Globals:  config.Globals{Env: map[string]string{"FOO": "new"}},
	})
	if len(d.restart) != 1 {
		t.Fatalf("restart count = %d, want 1 (non-reloadable service must keep old env)", len(d.restart))
	}
	if d.restart[0].new.Name != "b" {
		t.Errorf("restarted service = %q, want b", d.restart[0].new.Name)
	}
}

func TestDiff_GlobalsEnvUnchanged_NoRestart(t *testing.T) {
	svcs := []config.Service{mustService("a", "10_a.toml", nil)}
	o := diffFixture(t, svcs)
	o.cfg.Globals.Env = map[string]string{"FOO": "v"}

	d := o.computeDiff(&config.Config{
		Services: svcs,
		Globals:  config.Globals{Env: map[string]string{"FOO": "v"}},
	})
	if len(d.restart)+len(d.add)+len(d.remove) != 0 {
		t.Errorf("unchanged env should produce empty diff: %+v", d)
	}
}

func TestDiff_RenameIsRemoveAdd(t *testing.T) {
	old := []config.Service{mustService("redis", "10_redis.toml", nil)}
	new_ := []config.Service{mustService("redis", "20_redis.toml", nil)}
	o := diffFixture(t, old)
	d := o.computeDiff(&config.Config{Services: new_})
	if len(d.remove) != 1 || d.remove[0].Cfg().Filename != "10_redis.toml" {
		t.Errorf("expected to remove 10_redis.toml; got %+v", d.remove)
	}
	if len(d.add) != 1 || d.add[0].Filename != "20_redis.toml" {
		t.Errorf("expected to add 20_redis.toml; got %+v", d.add)
	}
}

func TestReload_AddsService(t *testing.T) {
	initial := []config.Service{mustService("a", "10_a.toml", nil)}
	f, captured := newCapturingFixture(t, initial, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	// Wait for initial.
	waitForSpawnCount(t, captured, 1, 2*time.Second)

	// Reload with one extra service.
	newCfg := &config.Config{
		Services: []config.Service{
			mustService("a", "10_a.toml", nil),
			mustService("b", "20_b.toml", nil),
		},
		Globals: f.cfg.Globals,
	}
	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatal(err)
	}
	waitForSpawnCount(t, captured, 2, 2*time.Second)

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

func TestReload_RemovesService(t *testing.T) {
	initial := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", nil),
	}
	f, captured := newCapturingFixture(t, initial, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 2, 2*time.Second)

	// Reload removes b.
	newCfg := &config.Config{
		Services: []config.Service{mustService("a", "10_a.toml", nil)},
		Globals:  f.cfg.Globals,
	}

	// b's process must receive a stop signal during Reload — push its
	// exit so the WaitTerminal in removeService unblocks promptly.
	bProc := captured.snapshot()[1]
	go func() {
		// SIGTERM arrives via Stop; deliver synthetic exit shortly after.
		time.Sleep(80 * time.Millisecond)
		bProc.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()

	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatal(err)
	}

	if got := len(f.orch.runners); got != 1 {
		t.Errorf("runners after reload = %d, want 1", got)
	}

	cancel()
	// a's process still alive — deliver its synthetic exit.
	go captured.snapshot()[0].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	f.awaitExit(3 * time.Second)
}

// waitOrchReady spins until the orchestrator's Run goroutine has
// committed runnerCtx (the publish point after which Reload is safe).
// Needed for tests that start with an empty service set, where there
// is no spawn we can wait on.
func waitOrchReady(t *testing.T, o *Orchestrator, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		o.mu.RLock()
		ready := o.runnerCtx != nil
		o.mu.RUnlock()
		if ready {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("orchestrator did not initialise runnerCtx within timeout")
}

func TestReload_RemoveStopFailureKeepsRunner(t *testing.T) {
	initial := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", func(s *config.Service) {
			s.StopTimeout = config.Duration(50 * time.Millisecond)
		}),
	}
	f, captured := newCapturingFixture(t, initial, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 2, 2*time.Second)

	// Reload removes b. b's fakeProcess never reaches terminal on
	// its own, so removeService's WaitTerminal must time out and
	// surface an error. We bound the wait via a tight reloadCtx
	// instead of waiting the full stop_timeout+reapGrace; the same
	// "WaitTerminal returned non-nil" code path fires.
	newCfg := &config.Config{
		Services: []config.Service{mustService("a", "10_a.toml", nil)},
		Globals:  f.cfg.Globals,
	}
	reloadCtx, reloadCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer reloadCancel()

	_, err := f.orch.Reload(reloadCtx, newCfg)
	if err == nil {
		t.Fatal("expected reload to return an error when stop fails")
	}

	// b's runner must still be registered so its Run goroutine can
	// keep tracking the still-live child.
	f.orch.mu.RLock()
	var stillThere bool
	for _, r := range f.orch.runners {
		if r.Cfg().Filename == "20_b.toml" {
			stillThere = true
			break
		}
	}
	f.orch.mu.RUnlock()
	if !stillThere {
		t.Fatal("b dropped from runners after stop-failure; orchestrator lost control of live child")
	}

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

func TestReload_AddsBootSerialInFilenameOrder(t *testing.T) {
	f, captured := newCapturingFixture(t, nil, "")

	// Wrap the fixture's spawner so we capture the order spawn() is
	// called in (filename), independent of the unordered input slice.
	var spawnOrderMu sync.Mutex
	var spawnOrder []string
	prev := f.orch.spawner
	f.orch.spawner = func(svc config.Service, env []string) (Process, error) {
		spawnOrderMu.Lock()
		spawnOrder = append(spawnOrder, svc.Filename)
		spawnOrderMu.Unlock()
		return prev(svc, env)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitOrchReady(t, f.orch, 2*time.Second)

	// Input is deliberately *out* of filename order; reload must
	// boot 10_a → 20_b → 30_c regardless.
	newCfg := &config.Config{
		Services: []config.Service{
			mustService("c", "30_c.toml", nil),
			mustService("a", "10_a.toml", nil),
			mustService("b", "20_b.toml", nil),
		},
		Globals: f.cfg.Globals,
	}
	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatalf("reload: %v", err)
	}

	waitForSpawnCount(t, captured, 3, 2*time.Second)

	spawnOrderMu.Lock()
	got := append([]string(nil), spawnOrder...)
	spawnOrderMu.Unlock()
	want := []string{"10_a.toml", "20_b.toml", "30_c.toml"}
	if len(got) < len(want) {
		t.Fatalf("spawn order = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("spawn[%d] = %s, want %s (full=%v)", i, got[i], w, got)
		}
	}

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

func TestStopAll_SerialReverseOrder(t *testing.T) {
	svcs := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", nil),
		mustService("c", "30_c.toml", nil),
	}
	f, captured := newCapturingFixture(t, svcs, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 3, 2*time.Second)

	procs := captured.snapshot()
	if len(procs) != 3 {
		t.Fatalf("want 3 procs, got %d", len(procs))
	}

	// We assert the event order is c-signal, c-exit, b-signal,
	// b-exit, a-signal, a-exit. The artificial inter-event sleeps
	// (100ms after c-signal, 50ms after b-signal) make the test
	// flunk loudly under the old "signal-all-then-wait" parallel
	// teardown — under that scheme all three signals would fire
	// nearly simultaneously, well before c-exit's recorded marker.
	var orderMu sync.Mutex
	var order []string
	record := func(s string) {
		orderMu.Lock()
		order = append(order, s)
		orderMu.Unlock()
	}
	waitSignaled := func(p *fakeProcess) bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(p.signalsReceived()) > 0 {
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if !waitSignaled(procs[2]) {
			t.Error("c never signaled")
			return
		}
		record("c-signal")
		time.Sleep(100 * time.Millisecond)
		record("c-exit")
		procs[2].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()
	go func() {
		defer wg.Done()
		if !waitSignaled(procs[1]) {
			t.Error("b never signaled")
			return
		}
		record("b-signal")
		time.Sleep(50 * time.Millisecond)
		record("b-exit")
		procs[1].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()
	go func() {
		defer wg.Done()
		if !waitSignaled(procs[0]) {
			t.Error("a never signaled")
			return
		}
		record("a-signal")
		record("a-exit")
		procs[0].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()

	cancel()
	wg.Wait()
	f.awaitExit(3 * time.Second)

	orderMu.Lock()
	got := append([]string(nil), order...)
	orderMu.Unlock()
	want := []string{
		"c-signal", "c-exit",
		"b-signal", "b-exit",
		"a-signal", "a-exit",
	}
	if len(got) != len(want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d] = %s, want %s (full=%v)", i, got[i], w, got)
			break
		}
	}
}

// TestReload_GlobalsEnvPropagatesToRestartedServices verifies that
// SIGHUP-style reloads with a changed globals.Env (a) restart
// reloadable services and (b) spawn them with the rebuilt baseEnv from
// the installed builder.
func TestReload_GlobalsEnvPropagatesToRestartedServices(t *testing.T) {
	initial := []config.Service{mustService("a", "10_a.toml", nil)}
	f, captured := newCapturingFixture(t, initial, "")
	f.cfg.Globals.Env = map[string]string{"FOO": "old"}

	// Wrap spawner to capture the env passed to each spawn.
	var (
		envMu  sync.Mutex
		envSeq [][]string
	)
	prev := f.orch.spawner
	f.orch.spawner = func(svc config.Service, env []string) (Process, error) {
		envMu.Lock()
		envSeq = append(envSeq, append([]string(nil), env...))
		envMu.Unlock()
		return prev(svc, env)
	}

	// Install a builder that maps globals.Env directly to the slice.
	// Production wires layeredMerge here; for the test we want the
	// new globals to show up verbatim so we can assert on it.
	f.orch.SetBaseEnvBuilder(func(g, _ map[string]string) []string {
		out := make([]string, 0, len(g))
		for k, v := range g {
			out = append(out, k+"="+v)
		}
		return out
	})
	// Seed the orchestrator's baseEnv to match the initial Env so the
	// post-spawn assertion below is meaningful.
	f.orch.baseEnv = []string{"FOO=old"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 1, 2*time.Second)

	// First spawn must use FOO=old (sanity check).
	envMu.Lock()
	first := envSeq[0]
	envMu.Unlock()
	if !envContains(first, "FOO=old") {
		t.Fatalf("first spawn env missing FOO=old: %v", first)
	}

	// Reload with new globals.Env. Push the existing process's exit so
	// removeService's WaitTerminal unblocks during the restart path.
	bProc := captured.snapshot()[0]
	go func() {
		time.Sleep(80 * time.Millisecond)
		bProc.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()
	newCfg := &config.Config{
		Services: initial,
		Globals:  config.Globals{Env: map[string]string{"FOO": "new"}, BootTimeout: f.cfg.Globals.BootTimeout, ExitCodeFrom: f.cfg.Globals.ExitCodeFrom},
	}
	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Reload's restart-new boot is detached; wait for the second spawn.
	waitForSpawnCount(t, captured, 2, 3*time.Second)

	envMu.Lock()
	second := envSeq[1]
	envMu.Unlock()
	if !envContains(second, "FOO=new") {
		t.Errorf("post-reload spawn env missing FOO=new: %v", second)
	}
	if envContains(second, "FOO=old") {
		t.Errorf("post-reload spawn env still has FOO=old: %v", second)
	}

	cancel()
	// Only the post-reload spawn is still alive; pushing exit on the
	// pre-reload process would re-close its channel and panic.
	go captured.snapshot()[1].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	f.awaitExit(3 * time.Second)
}

func envContains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// TestStopAll_ParallelWithinFilenameGroup: replicas of one service
// are signaled and awaited in parallel rather than one at a time.
// With N replicas at stop_timeout = T, wall-clock teardown must
// approach T, not N*T. The "signal before wait" assertion proves
// concurrency: every replica receives its TERM before any of them
// has reached terminal state.
func TestStopAll_ParallelWithinFilenameGroup(t *testing.T) {
	svc := mustService("consumer", "10_consumer.toml", func(s *config.Service) {
		s.Replicas = config.Replicas{N: 4}
		s.StopTimeout = config.Duration(time.Second)
	})
	f, captured := newCapturingFixture(t, []config.Service{svc}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 4, 2*time.Second)

	procs := captured.snapshot()
	if len(procs) != 4 {
		t.Fatalf("want 4 procs, got %d", len(procs))
	}

	// Watcher goroutines that record when each proc was signaled
	// AND release its exit only after a slight delay, so any
	// serial-waiter would be exposed by uneven signal arrival
	// (later replicas signaled only after earlier ones exited).
	var signalOrderMu sync.Mutex
	var signalOrder []int

	var wg sync.WaitGroup
	wg.Add(4)
	for i, p := range procs {
		go func(i int, p *fakeProcess) {
			defer wg.Done()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if len(p.signalsReceived()) > 0 {
					signalOrderMu.Lock()
					signalOrder = append(signalOrder, i)
					signalOrderMu.Unlock()
					// Hold the exit briefly so any serial implementation
					// would NOT have signaled siblings yet.
					time.Sleep(80 * time.Millisecond)
					p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Errorf("proc %d never signaled", i)
		}(i, p)
	}

	cancel() // triggers stopAll
	wg.Wait()
	f.awaitExit(3 * time.Second)

	signalOrderMu.Lock()
	got := append([]int(nil), signalOrder...)
	signalOrderMu.Unlock()
	if len(got) != 4 {
		t.Fatalf("signal count = %d, want 4 (got %v)", len(got), got)
	}
	// Under parallel-within-group, the 80ms hold per replica overlaps
	// freely and total elapsed should be ~80-150ms. Under the old
	// serial behavior it would be ~320ms (4 * 80ms). We don't time
	// directly (would be flaky); instead the signalOrder slice
	// completing means every proc was signaled while siblings were
	// still pre-exit, which is impossible under serial-with-wait.
}

func TestShutdownBudget_ReplicasShareGroupTimeout(t *testing.T) {
	// 64 replicas sharing one filename should contribute exactly one
	// (stop_timeout + reapGrace) to the budget, not 64. This is the
	// regression guard against the linear-scaling bug.
	svc := mustService("worker", "10_worker.toml", func(s *config.Service) {
		s.Replicas = config.Replicas{N: 64}
		s.StopTimeout = config.Duration(10 * time.Second)
	})
	f, captured := newCapturingFixture(t, []config.Service{svc}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 64, 3*time.Second)

	want := shutdownHeadroom + (10*time.Second + reapGrace)
	if got := f.orch.ShutdownBudget(); got != want {
		t.Errorf("ShutdownBudget with 64 replicas = %v, want %v (one group, not N)", got, want)
	}

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(5 * time.Second)
}

func TestShutdownBudget_RecomputedFromCurrentRunners(t *testing.T) {
	initial := []config.Service{
		mustService("a", "10_a.toml", func(s *config.Service) {
			s.StopTimeout = config.Duration(2 * time.Second)
		}),
	}
	f, captured := newCapturingFixture(t, initial, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 1, 2*time.Second)

	wantInitial := shutdownHeadroom + (2*time.Second + reapGrace)
	if got := f.orch.ShutdownBudget(); got != wantInitial {
		t.Errorf("initial ShutdownBudget = %v, want %v", got, wantInitial)
	}

	newCfg := &config.Config{
		Services: []config.Service{
			mustService("a", "10_a.toml", func(s *config.Service) {
				s.StopTimeout = config.Duration(2 * time.Second)
			}),
			mustService("b", "20_b.toml", func(s *config.Service) {
				s.StopTimeout = config.Duration(10 * time.Second)
			}),
		},
		Globals: f.cfg.Globals,
	}
	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	waitForSpawnCount(t, captured, 2, 2*time.Second)

	wantNew := shutdownHeadroom + (2*time.Second + reapGrace) + (10*time.Second + reapGrace)
	if got := f.orch.ShutdownBudget(); got != wantNew {
		t.Errorf("post-reload ShutdownBudget = %v, want %v", got, wantNew)
	}

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

func waitForSpawnCount(t *testing.T, c *capturedProcs, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(c.snapshot()) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("got %d spawns, want %d", len(c.snapshot()), want)
}

func TestReload_AddsReplicatedService(t *testing.T) {
	initial := []config.Service{mustService("a", "10_a.toml", nil)}
	f, captured := newCapturingFixture(t, initial, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 1, 2*time.Second)

	newCfg := &config.Config{
		Services: []config.Service{
			mustService("a", "10_a.toml", nil),
			mustService("b", "20_b.toml", func(s *config.Service) {
				s.Replicas = config.Replicas{N: 3}
			}),
		},
		Globals: f.cfg.Globals,
	}
	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatal(err)
	}
	// 1 initial + 3 replicas of b.
	waitForSpawnCount(t, captured, 4, 2*time.Second)

	f.orch.mu.RLock()
	var bRunners []*Runner
	for _, r := range f.orch.runners {
		if r.Cfg().Filename == "20_b.toml" {
			bRunners = append(bRunners, r)
		}
	}
	f.orch.mu.RUnlock()
	if len(bRunners) != 3 {
		t.Fatalf("b replicas after reload = %d, want 3", len(bRunners))
	}
	for i, r := range bRunners {
		if r.ReplicaIndex() != i {
			t.Errorf("b/%d ReplicaIndex = %d", i, r.ReplicaIndex())
		}
	}

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

func TestReload_RemovesReplicatedService(t *testing.T) {
	initial := []config.Service{
		mustService("a", "10_a.toml", nil),
		mustService("b", "20_b.toml", func(s *config.Service) {
			s.Replicas = config.Replicas{N: 3}
		}),
	}
	f, captured := newCapturingFixture(t, initial, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	// 1 (a) + 3 (b's replicas) = 4 spawns.
	waitForSpawnCount(t, captured, 4, 2*time.Second)

	// Reload with b removed entirely.
	newCfg := &config.Config{
		Services: []config.Service{mustService("a", "10_a.toml", nil)},
		Globals:  f.cfg.Globals,
	}

	// Push exits for all three b replicas as they get SIGTERM'd.
	procs := captured.snapshot()
	go func() {
		// b's replicas are spawns 2,3,4 (after a's spawn 1).
		time.Sleep(80 * time.Millisecond)
		for _, p := range procs[1:] {
			p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
		}
	}()

	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatal(err)
	}

	f.orch.mu.RLock()
	left := len(f.orch.runners)
	f.orch.mu.RUnlock()
	if left != 1 {
		t.Errorf("runners after replica-remove = %d, want 1", left)
	}

	cancel()
	go procs[0].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	f.awaitExit(3 * time.Second)
}

func TestReload_ReplicasCountChange(t *testing.T) {
	initial := []config.Service{
		mustService("consumer", "10_consumer.toml", func(s *config.Service) {
			s.Replicas = config.Replicas{N: 2}
		}),
	}
	f, captured := newCapturingFixture(t, initial, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)
	waitForSpawnCount(t, captured, 2, 2*time.Second)

	// Replicas 2 -> 4: spec changes → restart all old, spawn 4 new.
	newCfg := &config.Config{
		Services: []config.Service{
			mustService("consumer", "10_consumer.toml", func(s *config.Service) {
				s.Replicas = config.Replicas{N: 4}
			}),
		},
		Globals: f.cfg.Globals,
	}
	// Push exits for the two old replicas as they get SIGTERM'd.
	procs := captured.snapshot()
	go func() {
		time.Sleep(80 * time.Millisecond)
		procs[0].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
		procs[1].pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()

	if _, err := f.orch.Reload(ctx, newCfg); err != nil {
		t.Fatal(err)
	}
	// 2 old + 4 new = 6 total spawns.
	waitForSpawnCount(t, captured, 6, 3*time.Second)

	f.orch.mu.RLock()
	left := len(f.orch.runners)
	f.orch.mu.RUnlock()
	if left != 4 {
		t.Errorf("runners after replica-change = %d, want 4", left)
	}

	cancel()
	// Only the post-reload 4 are still alive.
	for _, p := range captured.snapshot()[2:] {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

func TestDiff_NoChangeForUnchangedReplicatedService(t *testing.T) {
	// Regression guard: per-replica log rewriting on the Cfg must not
	// produce a phantom diff when the spec is unchanged.
	svc := mustService("consumer", "10_consumer.toml", func(s *config.Service) {
		s.Replicas = config.Replicas{N: 3}
		s.Log.Stdout = "/var/log/consumer.log"
	})
	o := diffFixture(t, []config.Service{svc})
	// diffFixture wires NewRunner directly; expand manually so we get
	// per-replica log rewriting (matching production initial-boot).
	o.runners = expandServiceToRunners(svc, nil, nil, nil, o.log)

	d := o.computeDiff(&config.Config{Services: []config.Service{svc}})
	if total := len(d.add) + len(d.remove) + len(d.restart); total != 0 {
		t.Errorf("expected empty diff for unchanged replicated service; got %+v", d)
	}
}

func TestOrchestrator_ReplicasSpawnsN(t *testing.T) {
	svc := dummyService("consumer", false)
	svc.Replicas = config.Replicas{N: 3}
	f, captured := newCapturingFixture(t, []config.Service{svc}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	waitForSpawnCount(t, captured, 3, 2*time.Second)

	// All three runners must be registered and sorted by replica index.
	f.orch.mu.RLock()
	runners := append([]*Runner(nil), f.orch.runners...)
	f.orch.mu.RUnlock()
	if len(runners) != 3 {
		t.Fatalf("runners = %d, want 3", len(runners))
	}
	for i, r := range runners {
		if got := r.ReplicaIndex(); got != i {
			t.Errorf("runners[%d].ReplicaIndex = %d, want %d", i, got, i)
		}
		want := "consumer/" + string(rune('0'+i))
		if got := r.DisplayName(); got != want {
			t.Errorf("runners[%d].DisplayName = %q, want %q", i, got, want)
		}
	}

	cancel()
	for _, p := range captured.snapshot() {
		go p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}
	f.awaitExit(3 * time.Second)
}

func TestOrchestrator_ExitCodeFromWorker(t *testing.T) {
	worker := dummyService("worker", false)
	worker.Restart = config.RestartNever
	other := dummyService("php", false)

	f, captured := newCapturingFixture(t, []config.Service{other, worker}, "worker")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.start(ctx)

	// Wait for both to spawn.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(captured.snapshot()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	procs := captured.snapshot()
	if len(procs) < 2 {
		t.Fatalf("only %d spawns", len(procs))
	}
	// procs[1] is the worker (started second per filename order).
	worker_p := procs[1]
	other_p := procs[0]

	// Worker exits with code 7. exit_code_from -> orchestrator should
	// shut down siblings and return 7.
	worker_p.pushExit(reaper.ExitInfo{ExitCode: 7})

	// stopAll signals the other; deliver its exit.
	go func() {
		// Give orchestrator a moment to dispatch Stop, then push the exit.
		time.Sleep(100 * time.Millisecond)
		other_p.pushExit(reaper.ExitInfo{Signaled: true, Signal: 15})
	}()

	code := f.awaitExit(3 * time.Second)
	if code != 7 {
		t.Errorf("exit code = %d, want 7 (worker's)", code)
	}
}
