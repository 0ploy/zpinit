package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/machine"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/vsockexec"
)

// runWrapInit is wrap mode with --init, and every wrap in machine mode.
func runWrapInit(log *slog.Logger, cfg *config.Config, cmdline []string, env map[string]string, r *reaper.Reaper, opts runOpts) machine.Outcome {
	stop := wrapStop{timeout: cfg.Globals.DefaultStopTimeout.Std()}
	var cred *syscall.Credential
	var onStarted func()
	if ms := opts.machine; ms != nil {
		// The machine's graceful-stop contract replaces the container
		// one: the image's STOPSIGNAL, within the machine's budget.
		stop = wrapStop{signal: ms.StopSignal(), timeout: ms.Stop.Timeout.Std()}
		// zpinit stays root (as PID 1 it must be able to reboot); the
		// CMD runs as the image USER.
		c, err := identityFor(ms.User)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zpinit: user:", err)
			return machine.Outcome{Code: 126}
		}
		cred = c
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		onStarted = func() { startExecServer(ctx, log, ms, r) }
	}
	return runInitWrap(log, cmdline, env, cred, stop, opts.latch, opts.sink, r, onStarted)
}

// runPark is rescue mode: bringup failed and the bringup init parked
// the machine.
// No entrypoint.d, no services, no CMD, whatever argv and the config
// directory say, so a broken image cannot also break rescue. zpinit
// keeps reaping and serving exec until a graceful stop, which ends the
// machine with cause=parked.
func runPark(log *slog.Logger, ms *machine.Settings, latch *stopLatch) machine.Outcome {
	log.Warn("machine parked; rescue mode: no workload, waiting for exec or a stop",
		"step", ms.Rescue.Step, "error", ms.Rescue.Error)
	r := reaper.New(log)
	stopReap := startReapLoop(log, r)
	defer stopReap()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startExecServer(ctx, log, ms, r)
	<-latch.Done()
	log.Info("stop requested; ending the parked machine", "signal", latch.Signal())
	return machine.Outcome{Cause: machine.CauseParked}
}

// startExecServer opens the vsock exec listener when machine.json has an
// exec block. Only ever called in machine mode: vsock is not network-
// namespaced, so a listener opened in a container on a host that is
// itself a VM would be reachable from that VM's hypervisor. The reaper
// loop must already be running (exec children report through it).
func startExecServer(ctx context.Context, log *slog.Logger, ms *machine.Settings, r *reaper.Reaper) {
	if ms == nil || ms.Exec == nil {
		return
	}
	e := ms.Exec
	l, err := vsockexec.ListenVsock(e.Port)
	if err != nil {
		log.Error("exec listener not started", "port", e.Port, "err", err)
		return
	}
	srv := vsockexec.NewServer(vsockexec.Config{User: e.User, WorkingDir: e.WorkingDir, Env: e.Env}, r, log)
	log.Info("exec listener up", "transport", "vsock", "port", e.Port, "peer", "host CID only")
	go func() {
		if err := srv.Serve(ctx, l); err != nil {
			log.Error("exec listener failed", "err", err)
		}
	}()
}
