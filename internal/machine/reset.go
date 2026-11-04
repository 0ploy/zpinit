package machine

import (
	"fmt"
	"syscall"

	"github.com/0ploy/zpinit/internal/config"
)

// Cause is why the machine ends, as the reset line reports it.
type Cause string

const (
	// CauseExit: the workload ended on its own (the wrap child exited,
	// supervise mode ended, or zpinit failed before it got that far).
	CauseExit Cause = "exit"
	// CauseStop: a graceful stop was requested (Ctrl+Alt+Del, a stop
	// signal, zpctl shutdown).
	CauseStop Cause = "stop"
	// CauseParked: a parked (rescue) machine was stopped.
	CauseParked Cause = "parked"
)

// Outcome is how zpinit ends: the exit status it would report in a
// container, and why. Signal is non-zero when the wrap child was killed
// by a signal; Code then still carries the 128+N a container would see.
// OOM: the process whose status this is (the wrap CMD, or supervise
// mode's exit_code_from target) was killed by the kernel's OOM killer.
type Outcome struct {
	Cause  Cause
	Code   int
	Signal syscall.Signal
	OOM    bool
}

// ResetLinePrefix is what the host matches. A published, host-facing
// interface: never change it; an incompatible line gets a new prefix.
const ResetLinePrefix = "zpinit: reset "

// ResetLine renders the protocol line written to the console right
// before reboot(2), including the leading newline that keeps a partial
// line from the workload from swallowing it.
//
//	zpinit: reset intent=shutdown cause=exit status=0
//	zpinit: reset intent=shutdown cause=exit signal=SIGSEGV
//	zpinit: reset intent=shutdown cause=stop status=143
//	zpinit: reset intent=shutdown cause=parked
//	zpinit: reset intent=shutdown cause=exit signal=SIGKILL oom=1
func ResetLine(o Outcome) string {
	line := "\n" + ResetLinePrefix + "intent=shutdown cause=" + string(o.Cause)
	switch {
	case o.Cause == CauseParked:
		return line + "\n"
	case o.Signal != 0:
		line += " signal=" + config.SignalName(o.Signal)
	default:
		line += fmt.Sprintf(" status=%d", o.Code)
	}
	if o.OOM {
		line += " oom=1"
	}
	return line + "\n"
}
