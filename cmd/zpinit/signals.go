package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
)

// stopSignals request a graceful stop. SIGQUIT joins TERM/INT for
// supervisord parity: `docker kill -s QUIT` means "graceful shutdown"
// to operators coming from there, and the Go default (goroutine dump +
// exit 2) would be the hardest possible kill instead. In machine mode
// SIGINT is also how the kernel delivers Ctrl+Alt+Del.
//
// In machine mode SIGUSR1 and SIGUSR2 join them: busybox `halt` and
// `poweroff` signal PID 1 with those (and `reboot` with SIGTERM). The
// machine cannot halt or power off in place, so each ends it the one
// way it can end, a graceful stop; whether it comes back is the
// host's restart policy. In a container they stay ordinary signals,
// forwarded to an --init wrap child.
func stopSignals(isMachine bool) []os.Signal {
	sigs := []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT}
	if isMachine {
		sigs = append(sigs, syscall.SIGUSR1, syscall.SIGUSR2)
	}
	return sigs
}

// sinkSignals is every catchable signal that is not a stop signal and
// whose default action is not "ignore" or "stop". Without a Notify the
// Go runtime ends the process on several of them (HUP; and QUIT, ILL,
// TRAP, ABRT, BUS, FPE, SEGV, STKFLT, SYS when sent by kill), which for
// the kernel's init is a kernel panic and for a container an abrupt
// death. Left out: KILL and STOP (uncatchable), CHLD (the reaper's),
// URG (the runtime's preemption signal), CONT/TSTP/TTIN/TTOU/WINCH
// (default ignore or stop; catching TSTP would make a zpinit run by
// hand in a terminal immune to Ctrl+Z), and 32-34 (reserved for libc
// threading).
func sinkSignals(isMachine bool) []os.Signal {
	skip := map[syscall.Signal]bool{
		syscall.SIGKILL: true, syscall.SIGSTOP: true, syscall.SIGCHLD: true,
		syscall.SIGURG: true, syscall.SIGCONT: true, syscall.SIGTSTP: true,
		syscall.SIGTTIN: true, syscall.SIGTTOU: true, syscall.SIGWINCH: true,
	}
	for _, s := range stopSignals(isMachine) {
		skip[s.(syscall.Signal)] = true
	}
	maxSig := 31
	if runtime.GOOS == "linux" {
		maxSig = 64
	}
	var out []os.Signal
	for n := 1; n <= maxSig; n++ {
		if n >= 32 && n <= 34 {
			continue
		}
		if s := syscall.Signal(n); !skip[s] {
			out = append(out, s)
		}
	}
	return out
}

// signalSink catches sinkSignals for the life of the process. Signals go
// to the current forward function (the --init wrap child's process
// group), or nowhere. Notify rather than signal.Ignore: SIG_IGN would
// survive the wrap-mode exec and hand the CMD a polluted disposition
// table, while a caught signal resets to default across execve.
// runSupervise additionally Notifies SIGHUP onto its own channel for
// reload; duplicate delivery here is harmless.
type signalSink struct {
	forward atomic.Pointer[func(os.Signal)]
}

func installSignalSink(isMachine bool) *signalSink {
	s := &signalSink{}
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, sinkSignals(isMachine)...)
	go func() {
		for sig := range ch {
			if f := s.forward.Load(); f != nil {
				(*f)(sig)
			}
		}
	}()
	return s
}

func (s *signalSink) setForward(f func(os.Signal)) {
	if f == nil {
		s.forward.Store(nil)
		return
	}
	s.forward.Store(&f)
}

// stopLatch catches the stop signals from the moment it is installed
// and latches the first one. Every phase (entrypoint.d, the --init
// wrap, supervise, rescue) watches the same latch, so a stop that
// lands between two phases is never lost and never kills PID 1. Later
// stop signals are swallowed: graceful stop is already under way.
type stopLatch struct {
	done chan struct{}
	once sync.Once
	sig  atomic.Value // os.Signal
	// requested records any stop request, including `zpctl shutdown`,
	// which does not go through a signal. It picks cause=stop over
	// cause=exit for the machine-mode reset line.
	requested atomic.Bool
}

func installStopLatch(isMachine bool) *stopLatch {
	l := &stopLatch{done: make(chan struct{})}
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, stopSignals(isMachine)...)
	go func() {
		for sig := range ch {
			l.fire(sig)
		}
	}()
	return l
}

func (l *stopLatch) fire(sig os.Signal) {
	l.once.Do(func() {
		l.sig.Store(sig)
		l.requested.Store(true)
		close(l.done)
	})
}

// Done is closed once a stop signal has arrived.
func (l *stopLatch) Done() <-chan struct{} { return l.done }

// Signal returns the latched signal, or nil.
func (l *stopLatch) Signal() os.Signal {
	if v := l.sig.Load(); v != nil {
		return v.(os.Signal)
	}
	return nil
}

// Context is cancelled when the latch fires.
func (l *stopLatch) Context() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-l.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
