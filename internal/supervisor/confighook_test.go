package supervisor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/resources"
)

// hookRecorder collects the values the config-commit hook was called
// with. The hook fires synchronously inside Reload, but it is also
// reachable from the control-socket goroutine in production, so the
// recorder is mutex-guarded to keep -race honest.
type hookRecorder struct {
	mu   sync.Mutex
	vals []bool
}

func (h *hookRecorder) fn(needs bool) {
	h.mu.Lock()
	h.vals = append(h.vals, needs)
	h.mu.Unlock()
}

func (h *hookRecorder) snapshot() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.vals...)
}

// stopOnSignalProcess reaps itself the instant it is signalled.
//
// The shared fakeProcess waits for the test to push an exit, and these
// tests never advance the fake clock, so a runner that reached RUNNING
// would sit in STOPPING until removeServiceGroup's REAL-time budget
// (stop_timeout + reapGrace = 6s) expired. That made every reload step
// that removes a service a 6s flake, decided by whether the detached
// boot goroutine won the race to spawn before the removal started —
// which it does on Linux under -race and usually does not on macOS.
type stopOnSignalProcess struct{ *fakeProcess }

func (p *stopOnSignalProcess) SignalGroup(s syscall.Signal) error {
	err := p.fakeProcess.SignalGroup(s)
	p.pushExit(reaper.ExitInfo{Signaled: true, Signal: s})
	return err
}

// useSelfStoppingSpawner makes every spawned child terminate on its
// stop signal, so removals complete promptly regardless of how the
// boot race lands.
func useSelfStoppingSpawner(f *orchTestFixture) {
	var seq atomic.Int64
	f.orch.spawner = func(config.Service, []string) (Process, error) {
		return &stopOnSignalProcess{newFakeProcess(int(seq.Add(1)) + 3000)}, nil
	}
}

func reloadCfg(svcs ...config.Service) *config.Config {
	return &config.Config{
		Services: svcs,
		Globals: config.Globals{
			BootTimeout:  config.Duration(10 * time.Second),
			ExitCodeFrom: "default",
		},
	}
}

// The gate in cmd/zpinit starts the resource watcher lazily, so Reload
// must report whether the newly committed service set needs it. A
// container that boots with only static services and later gains an
// auto / reload_on_change service through `zpctl update` or SIGHUP
// would otherwise never start polling.
func TestConfigCommitHook_ReportsResourceWatchNeed(t *testing.T) {
	f := newOrchFixture(t, nil, "")
	useSelfStoppingSpawner(f)

	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.orch.mu.Lock()
	f.orch.runnerCtx = ctx
	f.orch.wg = &wg
	f.orch.mu.Unlock()

	rec := &hookRecorder{}
	f.orch.SetConfigCommitHook(rec.fn)

	static := dummyService("static", false)

	auto := dummyService("worker", false)
	auto.Replicas = config.Replicas{Auto: true, N: 1}

	onChange := dummyService("fpm", false)
	onChange.ReloadOnChange = []string{resources.DimCPU}

	steps := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"static only", reloadCfg(static), false},
		{"gains an auto service", reloadCfg(static, auto), true},
		{"auto removed again", reloadCfg(static), false},
		{"gains a reload_on_change service", reloadCfg(static, onChange), true},
	}
	for _, s := range steps {
		if _, err := f.orch.Reload(ctx, s.cfg); err != nil {
			t.Fatalf("%s: reload: %v", s.name, err)
		}
	}

	got := rec.snapshot()
	if len(got) != len(steps) {
		t.Fatalf("hook fired %d times, want %d (%v)", len(got), len(steps), got)
	}
	for i, s := range steps {
		if got[i] != s.want {
			t.Errorf("step %d (%s): hook got %v, want %v", i, s.name, got[i], s.want)
		}
	}
}

// ReloadScoped commits a config too, so it must notify as well: a
// `zpctl update worker` that introduces the first auto service has to
// arm the watcher exactly like a full update does.
func TestConfigCommitHook_FiresOnScopedUpdate(t *testing.T) {
	static := dummyService("static", false)
	f := newOrchFixture(t, []config.Service{static}, "")
	useSelfStoppingSpawner(f)

	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.orch.mu.Lock()
	f.orch.runnerCtx = ctx
	f.orch.wg = &wg
	f.orch.mu.Unlock()

	rec := &hookRecorder{}
	f.orch.SetConfigCommitHook(rec.fn)

	auto := dummyService("worker", false)
	auto.Replicas = config.Replicas{Auto: true, N: 1}

	if _, err := f.orch.ReloadScoped(ctx, reloadCfg(static, auto), []string{"worker"}); err != nil {
		t.Fatalf("scoped reload: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || !got[0] {
		t.Fatalf("scoped update adding an auto service: hook got %v, want [true]", got)
	}
}

// runSupervise starts the control socket BEFORE orch.Run publishes
// runnerCtx and wg, so a `zpctl update` can reach the reload path
// against a half-built orchestrator. That used to panic outright
// ("cannot create context from nil parent" in spawnRunnerGoroutine,
// then a nil-deref on the WaitGroup); the control handler's recover
// kept PID 1 alive but the reload was left half-applied. Registration
// must now refuse the batch cleanly instead.
func TestReloadBeforeRunIsRefusedNotPanicking(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setRunnerC bool
		setWG      bool
	}{
		{"nothing published", false, false},
		{"runnerCtx only", true, false},
		{"wg only", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrchFixture(t, nil, "")
			var wg sync.WaitGroup
			f.orch.mu.Lock()
			if tc.setRunnerC {
				f.orch.runnerCtx = context.Background()
			}
			if tc.setWG {
				f.orch.wg = &wg
			}
			f.orch.mu.Unlock()

			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("Reload panicked instead of refusing: %v", p)
				}
			}()
			_, err := f.orch.Reload(context.Background(),
				reloadCfg(dummyService("added", false)))
			if !errors.Is(err, errNotRunning) {
				t.Fatalf("err = %v, want errNotRunning", err)
			}
			// Nothing may be left registered: Run's setup rebuilds
			// o.runners from o.cfg, so a partial registration would be
			// silently dropped and the operator would see a reload that
			// reported success but started nothing.
			if got := len(f.orch.snapshotRunners()); got != 0 {
				t.Fatalf("%d runners registered by a refused reload, want 0", got)
			}
		})
	}
}

// An orchestrator with no hook installed is the test/default case and
// must not panic on the notify path.
func TestConfigCommitHook_UnsetIsSafe(t *testing.T) {
	f := newOrchFixture(t, nil, "")
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.orch.mu.Lock()
	f.orch.runnerCtx = ctx
	f.orch.wg = &wg
	f.orch.mu.Unlock()

	if _, err := f.orch.Reload(ctx, reloadCfg(dummyService("static", false))); err != nil {
		t.Fatalf("reload: %v", err)
	}
}
