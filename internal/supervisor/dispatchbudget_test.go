package supervisor

import (
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/ctlproto"
)

// A start --wait must get a dispatch budget large enough to cover the
// service reaching RUNNING (boot_timeout) plus its readiness probe;
// otherwise the daemon cancels the wait at the 30s floor and a healthy
// but slow service spuriously reports "not ready".
func TestDispatchBudget_StartWaitCoversBootAndProbe(t *testing.T) {
	const bootTimeout = 60 * time.Second
	svc := config.Service{
		Name: "api", Filename: "10_api.toml", Command: []string{"x"},
		StopTimeout: config.Duration(10 * time.Second),
		Ready:       &config.Ready{Timeout: config.Duration(30 * time.Second)},
	}
	o := &Orchestrator{
		log: testLog(),
		cfg: &config.Config{Globals: config.Globals{
			BootTimeout:  config.Duration(bootTimeout),
			ExitCodeFrom: "default",
		}},
	}
	o.runners = []*Runner{NewRunner(svc, nil, 0, nil, nil, testLog())}
	s := &ControlServer{orch: o, log: testLog()}

	plain := s.dispatchBudget(&ctlproto.Request{Verb: "start", Args: []string{"api"}})
	if plain != minDispatchBudget {
		t.Errorf("start (no --wait) budget = %v, want floor %v", plain, minDispatchBudget)
	}

	waitOne := s.dispatchBudget(&ctlproto.Request{Verb: "start", Args: []string{"--wait", "api"}})
	if waitOne <= bootTimeout {
		t.Errorf("start --wait api budget = %v, want > boot_timeout %v", waitOne, bootTimeout)
	}

	// `start --wait all` must resolve "all" rather than treating --wait
	// as a service name and collapsing to the floor budget.
	waitAll := s.dispatchBudget(&ctlproto.Request{Verb: "start", Args: []string{"--wait", "all"}})
	if waitAll <= bootTimeout {
		t.Errorf("start --wait all budget = %v, want > boot_timeout %v", waitAll, bootTimeout)
	}

	// restart --wait must cover stop AND boot+probe.
	restartWait := s.dispatchBudget(&ctlproto.Request{Verb: "restart", Args: []string{"--wait", "api"}})
	restartPlain := s.dispatchBudget(&ctlproto.Request{Verb: "restart", Args: []string{"api"}})
	if restartWait <= restartPlain {
		t.Errorf("restart --wait budget %v should exceed plain restart budget %v", restartWait, restartPlain)
	}
}
