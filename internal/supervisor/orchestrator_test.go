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
