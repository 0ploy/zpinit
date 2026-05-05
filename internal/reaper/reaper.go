// Package reaper centralizes wait(2) for the supervisor.
//
// As PID 1 zpinit must reap every child, including orphans reparented to us
// from anywhere in the container after a double-fork. Following the tini
// pattern there is exactly one syscall site (Reap), and per-process
// cmd.Wait is never used — that would race against this loop and lose exit
// codes when the kernel decides which caller to satisfy first.
package reaper

import (
	"errors"
	"log/slog"
	"syscall"
)

// Reaper is the centralized wait(2) loop. Phase 1 only logs reaps; later
// phases will add a PID→exit-channel map so service goroutines learn when
// their process has died.
type Reaper struct {
	log *slog.Logger
}

// New returns a Reaper that logs reapings via the given logger.
// If log is nil, slog.Default() is used.
func New(log *slog.Logger) *Reaper {
	if log == nil {
		log = slog.Default()
	}
	return &Reaper{log: log}
}

// Reap drains all currently-zombified children. Should be called on every
// SIGCHLD: one SIGCHLD can mean N dead children, so the loop is essential.
// Safe to call when no zombies are present.
func (r *Reaper) Reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err != nil {
			if errors.Is(err, syscall.ECHILD) {
				return
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			r.log.Error("wait4 failed", "err", err)
			return
		}
		if pid == 0 {
			return
		}
		switch {
		case ws.Exited():
			r.log.Info("reaped", "pid", pid, "code", ws.ExitStatus())
		case ws.Signaled():
			r.log.Info("reaped", "pid", pid, "signal", ws.Signal().String())
		}
	}
}
