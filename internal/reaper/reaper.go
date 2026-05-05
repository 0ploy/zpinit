// Package reaper centralizes wait(2) for the supervisor.
//
// As PID 1 zpinit must reap every child, including orphans reparented
// to us from anywhere in the container after a double-fork. Following
// the tini pattern there is exactly one syscall site (Reap), and
// per-process cmd.Wait is never used — that would race against this
// loop and lose exit codes when the kernel decided which caller to
// satisfy first.
//
// Service goroutines learn about their child's exit via a tracked
// channel registered atomically with cmd.Start (see SpawnTracked).
// Untracked reaps are logged as orphans.
package reaper

import (
	"errors"
	"log/slog"
	"os"
	"sync"
	"syscall"
)

// ExitInfo describes how a tracked child terminated. Either Signaled
// is true (and Signal is the killing signal), or ExitCode is the
// process exit status.
type ExitInfo struct {
	PID      int
	ExitCode int
	Signal   syscall.Signal
	Signaled bool
}

type Reaper struct {
	log *slog.Logger

	mu      sync.Mutex
	tracked map[int]chan<- ExitInfo
}

func New(log *slog.Logger) *Reaper {
	if log == nil {
		log = slog.Default()
	}
	return &Reaper{
		log:     log,
		tracked: make(map[int]chan<- ExitInfo),
	}
}

// SpawnTracked invokes spawn while holding the reaper's lock, then
// registers the resulting PID for exit notification. Holding the lock
// across cmd.Start closes the "Spawn-then-Track" race: if SIGCHLD
// arrives during spawn, Reap blocks on the same lock until tracking is
// recorded, after which the dispatch path finds the channel and sends
// ExitInfo on it. Without this, a fast-dying child could be reaped and
// logged as an orphan before the supervisor knew it was tracking it.
//
// spawn must call cmd.Start() (or otherwise create a child process) and
// return the *os.Process for it.
func (r *Reaper) SpawnTracked(spawn func() (*os.Process, error)) (*os.Process, <-chan ExitInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	proc, err := spawn()
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan ExitInfo, 1)
	r.tracked[proc.Pid] = ch
	return proc, ch, nil
}

// Untrack removes a PID from tracking. A subsequent reap of that PID
// will be logged as an orphan instead of dispatched. Use this when
// abandoning interest in a child (e.g. after deciding to SIGKILL and
// move on).
func (r *Reaper) Untrack(pid int) {
	r.mu.Lock()
	delete(r.tracked, pid)
	r.mu.Unlock()
}

// Reap drains all currently-zombified children. Should be called on
// every SIGCHLD: one SIGCHLD can mean N dead children, so the loop is
// essential. Safe to call when no zombies are present.
//
// Tracked PIDs receive their ExitInfo on the channel returned from
// SpawnTracked; untracked ones (true orphans, or PIDs explicitly
// Untracked) are logged.
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

		info := ExitInfo{PID: pid}
		switch {
		case ws.Exited():
			info.ExitCode = ws.ExitStatus()
		case ws.Signaled():
			info.Signaled = true
			info.Signal = ws.Signal()
		}

		r.mu.Lock()
		ch, tracked := r.tracked[pid]
		if tracked {
			delete(r.tracked, pid)
		}
		r.mu.Unlock()

		if tracked {
			ch <- info
			close(ch)
			continue
		}
		if info.Signaled {
			r.log.Info("reaped orphan", "pid", pid, "signal", info.Signal.String())
		} else {
			r.log.Info("reaped orphan", "pid", pid, "code", info.ExitCode)
		}
	}
}
