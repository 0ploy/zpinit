package supervisor

import (
	"syscall"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/service"
)

// Process is the surface the Runner needs from a spawned child. The
// concrete implementation in production is a thin adapter over
// *service.Process; tests provide an in-memory fake.
type Process interface {
	PID() int
	Exit() <-chan reaper.ExitInfo
	SignalGroup(sig syscall.Signal) error
}

// Spawner is the dependency the Runner uses to create new processes.
// Production binds it to service.Spawn via WrapServiceProcess; tests
// inject their own to feed synthetic Exit events.
type Spawner func(cfg config.Service, env []string) (Process, error)

// OneShotSpawner runs a transient command outside the per-service
// state machine: a reload_command, mostly. Returns a channel that
// fires once with the ExitInfo when the kernel reaps the child.
// Production wires this to service.SpawnOneShot via the
// centralized reaper; tests inject a fake that drives Exit
// synthetically.
type OneShotSpawner func(name string, command, env []string) (<-chan reaper.ExitInfo, error)

// WrapServiceProcess adapts a *service.Process to the Process interface.
// service.Process exposes PID/Exit as fields rather than methods, so
// this adapter bridges the two.
func WrapServiceProcess(p *service.Process) Process {
	return &serviceProcessAdapter{p: p}
}

type serviceProcessAdapter struct{ p *service.Process }

func (a *serviceProcessAdapter) PID() int                           { return a.p.PID }
func (a *serviceProcessAdapter) Exit() <-chan reaper.ExitInfo       { return a.p.Exit }
func (a *serviceProcessAdapter) SignalGroup(s syscall.Signal) error { return a.p.SignalGroup(s) }
