package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/entrypoint"
	"github.com/0ploy/zpinit/internal/execpath"
	"github.com/0ploy/zpinit/internal/machine"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/service"
)

// startReapLoop runs the single wait4 site on every SIGCHLD until the
// returned stop function is called (which does a final drain). Reap is
// wrapped with recover() so an unforeseen panic doesn't take the
// SIGCHLD handler offline (which would silently wedge every waiter on
// a child's Exit channel); the next SIGCHLD retries.
func startReapLoop(log *slog.Logger, r *reaper.Reaper) (stop func()) {
	chldCh := make(chan os.Signal, 16)
	signal.Notify(chldCh, syscall.SIGCHLD)
	stopCh := make(chan struct{})
	done := make(chan struct{})
	safeReap := func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("reaper panic; continuing", "panic", p)
			}
		}()
		r.Reap()
	}
	go func() {
		defer close(done)
		// A child that died before Notify was installed sent its
		// SIGCHLD into the void; drain once up front.
		safeReap()
		for {
			select {
			case <-chldCh:
				safeReap()
			case <-stopCh:
				safeReap() // final drain
				return
			}
		}
	}()
	return func() {
		close(stopCh)
		<-done
		signal.Stop(chldCh)
	}
}

// runInitWrap is wrap mode with --init, and every wrap in machine mode:
// it settles how the child is stopped and who it runs as, then hands
// over to runResidentWrap.
func runInitWrap(log *slog.Logger, cfg *config.Config, cmdline []string, env map[string]string, r *reaper.Reaper, opts runOpts) machine.Outcome {
	stop := wrapStop{timeout: cfg.Globals.DefaultStopTimeout.Std()}
	var cred *syscall.Credential
	var onStarted func()
	if ms := opts.machine; ms != nil {
		// The machine's graceful-stop contract replaces the container
		// one: the image's STOPSIGNAL, within the machine's budget.
		stop = wrapStop{signal: ms.StopSignal(), timeout: ms.Stop.Timeout.Std()}
		// zpinit stays root (as PID 1 it must be able to reboot); the
		// CMD runs as the image USER.
		id, err := service.ResolveIdentity(ms.User)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zpinit: user:", err)
			return machine.Outcome{Code: 126}
		}
		cred = id.Cred
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		onStarted = func() { startExecServer(ctx, log, ms, r) }
	}
	return runResidentWrap(log, cmdline, env, cred, stop, opts.latch, opts.sink, r, onStarted)
}

// wrapStop is how the --init wrap stops its child.
type wrapStop struct {
	// signal overrides the forwarded stop signal (machine mode's
	// stop.signal). Zero forwards the signal zpinit received.
	signal  syscall.Signal
	timeout time.Duration
}

// runResidentWrap is the resident wrap (--init, forced in machine
// mode): fork the CMD instead of exec'ing it and stay resident above it, the way
// tini does. zpinit keeps reaping (orphans included), forwards signals
// to the child's process group, turns a stop signal into a graceful
// stop bounded by the stop timeout, and ends with the child's status
// (128+N for a signal).
//
// cred is the child's identity (machine mode's `user`); nil inherits
// zpinit's.
func runResidentWrap(log *slog.Logger, cmdline []string, env map[string]string, cred *syscall.Credential,
	stop wrapStop, latch *stopLatch, sink *signalSink, r *reaper.Reaper, onStarted func()) machine.Outcome {
	// Resolve against the merged env's PATH, as execCmd does, without
	// touching PID 1's own: zpinit stays resident here.
	argv0, err := execpath.Find(cmdline[0], childPath(env))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zpinit:", err)
		return machine.Outcome{Code: 127}
	}

	stopReap := startReapLoop(log, r)
	defer stopReap()
	if onStarted != nil {
		onStarted()
	}

	cmd := exec.Command(argv0, cmdline[1:]...)
	cmd.Args = cmdline
	cmd.Env = entrypoint.SliceFromEnviron(env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Its own process group, so forwarded signals and the final SIGKILL
	// reach forks of the CMD too. When stdin is zpinit's controlling
	// terminal the group is put in the foreground, so Ctrl+C and job
	// control behave as if the CMD were run directly. A tty that is not
	// the controlling terminal (/dev/console under init=zpinit) is left
	// alone: asking for the foreground there fails the fork.
	cmd.SysProcAttr = wrapSysProcAttr(controlsTerminal(0))
	if cred != nil {
		cmd.SysProcAttr.Credential = cred
	}

	proc, exitCh, err := r.SpawnTracked(func() (*os.Process, error) {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zpinit: start:", err)
		return machine.Outcome{Code: 126}
	}
	pid := proc.Pid
	log.Info("started", "cmd", cmdline, "pid", pid)

	// Everything the sink catches goes on to the CMD's group, as tini
	// does, except SIGPIPE: that one is usually the kernel's reaction
	// to zpinit's own write to a closed stdout/stderr, not a message
	// for the workload.
	sink.setForward(func(sig os.Signal) {
		if sig == syscall.SIGPIPE {
			return
		}
		_ = syscall.Kill(-pid, sig.(syscall.Signal))
	})
	defer sink.setForward(nil)

	var (
		latched  = latch.Done()
		killTime <-chan time.Time
	)
	for {
		select {
		case info := <-exitCh:
			if info.OOMKilled {
				log.Warn("the CMD was killed by the kernel's OOM killer", "pid", pid)
			}
			return outcomeOf(info)
		case <-latched:
			latched = nil
			sig := stop.signal
			if sig == 0 {
				sig, _ = latch.Signal().(syscall.Signal)
			}
			if sig == 0 {
				sig = syscall.SIGTERM
			}
			log.Info("stopping", "signal", config.SignalName(sig), "timeout", stop.timeout, "pid", pid)
			_ = syscall.Kill(-pid, sig)
			t := time.NewTimer(stop.timeout)
			defer t.Stop()
			killTime = t.C
		case <-killTime:
			killTime = nil
			log.Warn("stop timeout elapsed; SIGKILL", "pid", pid, "timeout", stop.timeout)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}

func outcomeOf(info reaper.ExitInfo) machine.Outcome {
	if info.Signaled {
		return machine.Outcome{Code: 128 + int(info.Signal), Signal: info.Signal, OOM: info.OOMKilled}
	}
	return machine.Outcome{Code: info.ExitCode}
}
