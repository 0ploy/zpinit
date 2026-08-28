package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// Initial boot is all-or-nothing by default: one service that cannot
// come up aborts PID 1, so an orchestrator sees a container that failed
// to start rather than one idling while looking healthy. A dev
// container wants the opposite, because `docker exec` has to survive a
// broken app long enough to repair it in place. on_boot_failure picks
// per service; these tests pin both sides.

// bootFixture runs o.boot against services whose children die
// immediately, which is the deterministic form of "failed to boot":
// restart = "never" plus a nonzero exit settles in Stopped, so
// WaitBootResult fails at once instead of burning boot_timeout.
func bootFixture(t *testing.T, svcs ...config.Service) *Orchestrator {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	o := &Orchestrator{
		log: testLog(),
		cfg: &config.Config{
			Globals:  config.Globals{ExitCodeFrom: "default", BootTimeout: config.Duration(2 * time.Second)},
			Services: svcs,
		},
	}
	o.runnerCtx = ctx
	o.wg = &wg
	o.earlyShutdownCh = make(chan struct{})
	for _, svc := range svcs {
		svc := svc
		r := NewRunner(svc, nil, 0, func(config.Service, []string) (Process, error) {
			p := newFakeProcess(2000)
			if svc.Env["fail"] == "1" {
				p.pushExit(reaper.ExitInfo{ExitCode: 7})
			}
			return p, nil
		}, newFakeClock(time.Now()), testLog())
		r.jitterRand = nil
		o.runners = append(o.runners, r)
		o.spawnRunnerGoroutine(r)
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return o
}

func failingSvc(name, filename string, onFail config.OnBootFailure) config.Service {
	return config.Service{
		Name: name, Filename: filename, Command: []string{name},
		Restart: config.RestartNever, StopSignal: "TERM",
		StopTimeout:   config.Duration(time.Second),
		OnBootFailure: onFail,
		Env:           map[string]string{"fail": "1"},
	}
}

func healthySvc(name, filename string) config.Service {
	return config.Service{
		Name: name, Filename: filename, Command: []string{name},
		Restart: config.RestartAlways, StopSignal: "TERM",
		StopTimeout:   config.Duration(time.Second),
		OnBootFailure: config.BootFail,
	}
}

// TestBoot_OnBootFailureFail pins the default: the container dies.
func TestBoot_OnBootFailureFail(t *testing.T) {
	o := bootFixture(t, failingSvc("app", "10_app.toml", config.BootFail))
	if err := o.boot(context.Background()); err == nil {
		t.Fatal("boot() = nil, want an error so PID 1 aborts")
	}
}

// TestBoot_OnBootFailureContinue is the dev-container case: the app
// cannot start, and the container comes up anyway so an operator can
// exec in and fix it.
func TestBoot_OnBootFailureContinue(t *testing.T) {
	o := bootFixture(t, failingSvc("app", "10_app.toml", config.BootContinue))
	if err := o.boot(context.Background()); err != nil {
		t.Fatalf("boot() = %v, want nil so the container stays up", err)
	}
	// Still registered and visible, so `zpctl status` reports the
	// failure rather than hiding it.
	if n := len(o.snapshotRunners()); n != 1 {
		t.Errorf("registered runners = %d, want 1", n)
	}
}

// TestBoot_OnBootFailureContinueBootsLaterServices checks that opting
// one service out does not strand the services behind it. Filename
// order is a start order, not a dependency graph.
func TestBoot_OnBootFailureContinueBootsLaterServices(t *testing.T) {
	o := bootFixture(t,
		failingSvc("app", "10_app.toml", config.BootContinue),
		healthySvc("nginx", "20_nginx.toml"),
	)
	if err := o.boot(context.Background()); err != nil {
		t.Fatalf("boot() = %v, want nil", err)
	}
	snap := o.snapshotRunners()
	if got := snap[1].State(); got != StateRunning {
		t.Errorf("service behind the failed one: state = %s, want %s", got, StateRunning)
	}
}

// TestBoot_OnBootFailureIsPerService confirms the opt-out does not leak:
// a later service still on the default aborts the boot even though an
// earlier one was allowed to fail.
func TestBoot_OnBootFailureIsPerService(t *testing.T) {
	o := bootFixture(t,
		failingSvc("app", "10_app.toml", config.BootContinue),
		failingSvc("worker", "20_worker.toml", config.BootFail),
	)
	if err := o.boot(context.Background()); err == nil {
		t.Fatal("boot() = nil, want an error from the service still on on_boot_failure = fail")
	}
}
