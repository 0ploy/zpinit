package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
)

// startedRunner returns a Runner already driven to Running, plus a
// cancel that stops its Run goroutine.
func startedRunner(t *testing.T, cfg config.Service) (*Runner, *fakeClock, context.CancelFunc) {
	t.Helper()
	clk := newFakeClock(time.Unix(0, 0))
	procs := make(chan *fakeProcess, 4)
	spawn := func(_ config.Service, _ []string) (Process, error) {
		p := newFakeProcess(1000)
		procs <- p
		return p, nil
	}
	r := NewRunner(cfg, nil, 0, spawn, clk, testLog())
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	if err := r.StartCtx(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	<-procs
	if !waitFor(time.Second, func() bool { return r.State() == StateRunning }) {
		cancel()
		t.Fatalf("runner did not reach Running")
	}
	return r, clk, cancel
}

func TestWaitUntilReady_NoProbeSucceeds(t *testing.T) {
	cfg := config.Service{Name: "api", Filename: "10_api.toml", Command: []string{"x"},
		Restart: config.RestartAlways, StopSignal: "TERM", StopTimeout: config.Duration(time.Second)}
	r, _, cancel := startedRunner(t, cfg)
	defer cancel()

	o := &Orchestrator{
		log: testLog(),
		cfg: &config.Config{Globals: config.Globals{BootTimeout: config.Duration(time.Second), ExitCodeFrom: "default"}},
	}
	if err := o.WaitUntilReady(context.Background(), r); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if !r.ReadyPassed() {
		t.Errorf("expected ReadyPassed after WaitUntilReady on a no-probe service")
	}
}

func TestWaitUntilReady_TimesOutBeforeRunning(t *testing.T) {
	// A Pending runner that never starts: WaitUntilReady must surface
	// the ctx deadline rather than block.
	cfg := config.Service{Name: "api", Filename: "10_api.toml", Command: []string{"x"},
		Restart: config.RestartAlways, StopSignal: "TERM", StopTimeout: config.Duration(time.Second)}
	r := NewRunner(cfg, nil, 0, nil, nil, testLog())

	o := &Orchestrator{
		log: testLog(),
		cfg: &config.Config{Globals: config.Globals{BootTimeout: config.Duration(time.Second), ExitCodeFrom: "default"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := o.WaitUntilReady(ctx, r); err == nil {
		t.Fatal("expected a timeout error for a runner that never reaches Running")
	}
}

func TestReloadScoped_AddsOnlyNamedDefersGlobals(t *testing.T) {
	aSvc := config.Service{Name: "a", Filename: "10_a.toml", Command: []string{"a"},
		Restart: config.RestartAlways, StopSignal: "TERM", StopTimeout: config.Duration(500 * time.Millisecond)}

	clk := newFakeClock(time.Unix(0, 0))
	procs := make(chan *fakeProcess, 16)
	var mu sync.Mutex
	spawn := func(_ config.Service, _ []string) (Process, error) {
		mu.Lock()
		defer mu.Unlock()
		p := newFakeProcess(2000 + len(procs))
		procs <- p
		return p, nil
	}

	ra := NewRunner(aSvc, nil, 0, spawn, clk, testLog())
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go ra.Run(runCtx)
	if err := ra.StartCtx(runCtx); err != nil {
		t.Fatalf("start a: %v", err)
	}
	<-procs

	o := &Orchestrator{
		log:       testLog(),
		spawner:   spawn,
		clock:     clk,
		wg:        &sync.WaitGroup{},
		runnerCtx: runCtx,
		cfg: &config.Config{
			Services: []config.Service{aSvc},
			Globals:  config.Globals{ExitCodeFrom: "default", Env: map[string]string{"FOO": "1"}},
		},
	}
	o.runners = []*Runner{ra}

	// New disk config: a unchanged, b + c added, global env changed.
	bSvc := config.Service{Name: "b", Filename: "20_b.toml", Command: []string{"b"},
		Restart: config.RestartAlways, StopSignal: "TERM", StopTimeout: config.Duration(500 * time.Millisecond)}
	cSvc := config.Service{Name: "c", Filename: "30_c.toml", Command: []string{"c"},
		Restart: config.RestartAlways, StopSignal: "TERM", StopTimeout: config.Duration(500 * time.Millisecond)}
	newCfg := &config.Config{
		Services: []config.Service{aSvc, bSvc, cSvc},
		Globals:  config.Globals{ExitCodeFrom: "default", Env: map[string]string{"FOO": "2"}},
	}

	diff, err := o.ReloadScoped(context.Background(), newCfg, []string{"b"})
	if err != nil {
		t.Fatalf("ReloadScoped: %v", err)
	}

	if len(diff.add) != 1 || diff.add[0].Name != "b" {
		t.Fatalf("diff.add = %v, want just [b]", diff.add)
	}
	if len(diff.remove) != 0 || len(diff.restart) != 0 {
		t.Errorf("scoped add should not remove/restart anything: %+v", diff)
	}

	names := map[string]bool{}
	for _, r := range o.snapshotRunners() {
		names[r.Cfg().Name] = true
	}
	if !names["a"] || !names["b"] {
		t.Errorf("runners after scoped update = %v, want a and b present", names)
	}
	if names["c"] {
		t.Errorf("c was NOT named; scoped update must not add it: %v", names)
	}

	// Global [env] change is deferred, not applied.
	if got := o.GlobalsEnv()["FOO"]; got != "1" {
		t.Errorf("globals FOO = %q after scoped update, want 1 (deferred)", got)
	}
	// Committed cfg has a and b, not the unnamed c.
	o.mu.RLock()
	committed := map[string]bool{}
	for _, s := range o.cfg.Services {
		committed[s.Name] = true
	}
	o.mu.RUnlock()
	if !committed["a"] || !committed["b"] || committed["c"] {
		t.Errorf("committed services = %v, want {a,b} only", committed)
	}
}

func TestReloadScoped_UnknownNameChangesNothing(t *testing.T) {
	aSvc := config.Service{Name: "a", Filename: "10_a.toml", Command: []string{"a"},
		Restart: config.RestartAlways, StopSignal: "TERM", StopTimeout: config.Duration(500 * time.Millisecond)}
	o := &Orchestrator{
		log:       testLog(),
		wg:        &sync.WaitGroup{},
		runnerCtx: context.Background(),
		cfg: &config.Config{
			Services: []config.Service{aSvc},
			Globals:  config.Globals{ExitCodeFrom: "default"},
		},
	}
	o.runners = []*Runner{NewRunner(aSvc, nil, 0, nil, nil, testLog())}

	newCfg := &config.Config{Services: []config.Service{aSvc}, Globals: o.cfg.Globals}
	_, err := o.ReloadScoped(context.Background(), newCfg, []string{"ghost"})
	if err == nil || !errors.Is(err, errUnknownService) {
		t.Fatalf("ReloadScoped(ghost) error = %v, want errUnknownService", err)
	}
	if len(o.snapshotRunners()) != 1 {
		t.Errorf("runner set should be unchanged after an unknown-name update")
	}
}

func TestCmdResolve(t *testing.T) {
	root := t.TempDir()
	sdir := filepath.Join(root, "services")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "10_redis.toml"), []byte(`command=["redis"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "20_worker.toml.disabled"), []byte(`command=["w"]`), 0o644); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{log: testLog(), cfg: &config.Config{Dir: root, Globals: config.Globals{ExitCodeFrom: "default"}}}
	s := &ControlServer{orch: o, log: testLog()}

	// Enabled service resolves to its path with enabled=true.
	resp := s.cmdResolve([]string{"redis"})
	if resp.Code != 0 || len(resp.Body) != 1 {
		t.Fatalf("resolve redis: code=%d body=%v", resp.Code, resp.Body)
	}
	var got struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(resp.Body[0]), &got); err != nil {
		t.Fatalf("resolve output not JSON: %v (%s)", err, resp.Body[0])
	}
	if got.Name != "redis" || !got.Enabled || filepath.Base(got.Path) != "10_redis.toml" {
		t.Errorf("resolve redis = %+v", got)
	}

	// Disabled service still resolves, enabled=false.
	resp = s.cmdResolve([]string{"worker"})
	if resp.Code != 0 {
		t.Fatalf("resolve worker: code=%d", resp.Code)
	}
	_ = json.Unmarshal([]byte(resp.Body[0]), &got)
	if got.Name != "worker" || got.Enabled {
		t.Errorf("resolve worker = %+v, want enabled=false", got)
	}

	// Unknown service -> CodeUnknownService.
	resp = s.cmdResolve([]string{"ghost"})
	if resp.Code != 3 {
		t.Errorf("resolve ghost: code=%d, want 3 (unknown service)", resp.Code)
	}
}
