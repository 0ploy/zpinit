package config

import (
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
}

// ParseSignal returns the syscall.Signal for the given name, accepting
// either "TERM" or "SIGTERM" (case-insensitive). The bool reports whether
// the name was recognised.
func ParseSignal(name string) (syscall.Signal, bool) {
	n := strings.TrimPrefix(strings.ToUpper(name), "SIG")
	s, ok := signalsByName[n]
	return s, ok
}
