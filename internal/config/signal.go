package config

import (
	"fmt"
	"strings"
	"syscall"
)

// signalsByName maps the bare name (without SIG prefix) to the syscall
// signal. Both "TERM" and "SIGTERM" are accepted in configs and on the
// zpctl signal command line.
var signalsByName = map[string]syscall.Signal{
	"HUP":   syscall.SIGHUP,
	"INT":   syscall.SIGINT,
	"QUIT":  syscall.SIGQUIT,
	"ABRT":  syscall.SIGABRT,
	"KILL":  syscall.SIGKILL,
	"USR1":  syscall.SIGUSR1,
	"USR2":  syscall.SIGUSR2,
	"PIPE":  syscall.SIGPIPE,
	"ALRM":  syscall.SIGALRM,
	"TERM":  syscall.SIGTERM,
	"CHLD":  syscall.SIGCHLD,
	"CONT":  syscall.SIGCONT,
	"STOP":  syscall.SIGSTOP,
	"TSTP":  syscall.SIGTSTP,
	"URG":   syscall.SIGURG,
	"WINCH": syscall.SIGWINCH,
	// The rest of the standard set. Accepted so a stop_signal (or an
	// image STOPSIGNAL handed over in machine.json) naming any of
	// them parses, and so SignalName can name any signal a child dies
	// of.
	"ILL":    syscall.SIGILL,
	"TRAP":   syscall.SIGTRAP,
	"BUS":    syscall.SIGBUS,
	"FPE":    syscall.SIGFPE,
	"SEGV":   syscall.SIGSEGV,
	"TTIN":   syscall.SIGTTIN,
	"TTOU":   syscall.SIGTTOU,
	"XCPU":   syscall.SIGXCPU,
	"XFSZ":   syscall.SIGXFSZ,
	"VTALRM": syscall.SIGVTALRM,
	"PROF":   syscall.SIGPROF,
	"IO":     syscall.SIGIO,
	"SYS":    syscall.SIGSYS,
}

// namesBySignal is the reverse of signalsByName, built in init so the
// per-OS additions (signal_linux.go) are in it.
var namesBySignal map[syscall.Signal]string

func init() {
	for n, s := range osSignals {
		signalsByName[n] = s
	}
	namesBySignal = make(map[syscall.Signal]string, len(signalsByName))
	for n, s := range signalsByName {
		namesBySignal[s] = "SIG" + n
	}
}

// SignalName returns the conventional name of sig ("SIGTERM"), or
// "SIG<n>" for a signal with no name (the realtime range). Unlike
// syscall.Signal.String it is the token a config or a host-side
// matcher uses, not a description.
func SignalName(sig syscall.Signal) string {
	if n, ok := namesBySignal[sig]; ok {
		return n
	}
	return fmt.Sprintf("SIG%d", int(sig))
}

// ParseSignal returns the syscall.Signal for the given name, accepting
// either "TERM" or "SIGTERM" (case-insensitive). The bool reports whether
// the name was recognised.
func ParseSignal(name string) (syscall.Signal, bool) {
	n := strings.TrimPrefix(strings.ToUpper(name), "SIG")
	s, ok := signalsByName[n]
	return s, ok
}
