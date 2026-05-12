package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

// Prober runs a readiness probe command and returns nil if the probe
// reports the service is ready, or an error otherwise. It is the
// orchestrator's only shape of dependency on actual subprocesses
// during the boot phase, which lets tests drop in a fake.
type Prober func(ctx context.Context, cmd []string, env []string, cwd string) error

// probeNull is a write-only handle to /dev/null shared by every probe
// invocation. We don't use io.Discard because exec.Cmd allocates pipes
// (and goroutines) for any non-*os.File stdout/stderr, and the
// centralized-reaper rule forbids cmd.Wait() — which is what would
// otherwise close those pipes. Result: per-probe FD leak until GC.
// Opening /dev/null once and reusing it sidesteps both the pipe and
// the FD lifetime problem. Intentionally never closed; the kernel
// reclaims on process exit.
var (
	probeNullOnce sync.Once
	probeNull     *os.File
	probeNullErr  error
)

// probeReapGiveUp bounds how long the prober waits for SIGCHLD after
// it has issued SIGKILL on a canceled probe. If the probe child is
// pinned in uninterruptible kernel sleep (D state), the kernel can't
// deliver SIGKILL until the syscall completes; without this bound,
// `<-exitCh` would block indefinitely and wedge the entire boot path.
// The reaper's `tracked` channel is buffered (cap=1) so a later reap
// still completes correctly even after we abandon the wait.
const probeReapGiveUp = 5 * time.Second

func openProbeNull() (*os.File, error) {
	probeNullOnce.Do(func() {
		probeNull, probeNullErr = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	})
	return probeNull, probeNullErr
}

// defaultProber spawns the probe command via the centralized reaper.
// stdout/stderr are discarded — probes are noisy and the result is
// already captured in the exit code.
func defaultProber(r *reaper.Reaper, log *slog.Logger) Prober {
	return func(ctx context.Context, cmd []string, env []string, cwd string) error {
		if len(cmd) == 0 {
			return errors.New("empty probe command")
		}
		// /dev/null is the only safe sink: io.Discard makes exec.Cmd
		// allocate pipes + reader goroutines, and the centralized-reaper
		// rule forbids cmd.Wait (which is what would close them) — net
		// result one FD + one goroutine leak per probe. Refuse the probe
		// rather than leaking; in any real container /dev/null exists.
		devNull, err := openProbeNull()
		if err != nil {
			return fmt.Errorf("open /dev/null for probe: %w", err)
		}
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Env = env
		c.Dir = cwd
		c.Stdin = nil
		c.Stdout = devNull
		c.Stderr = devNull
		// Setpgid so a single Kill(-pid) reaches the probe and any
		// subcommands it spawns. No Pdeathsig — probe is short-lived.
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		proc, exitCh, err := r.SpawnTracked(func() (*os.Process, error) {
			if err := c.Start(); err != nil {
				return nil, err
			}
			return c.Process, nil
		})
		if err != nil {
			return fmt.Errorf("start probe: %w", err)
		}

		select {
		case info := <-exitCh:
			if info.Signaled {
				return fmt.Errorf("probe killed by %s", info.Signal)
			}
			if info.ExitCode != 0 {
				return fmt.Errorf("probe exited %d", info.ExitCode)
			}
			return nil
		case <-ctx.Done():
			_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
			// Bounded wait: a D-state probe will not deliver SIGCHLD
			// until its kernel I/O completes, which can be arbitrarily
			// long. Abandoning the channel is safe because the reaper
			// map entry's buffered (cap=1) channel absorbs the eventual
			// send and is then GC'd.
			select {
			case <-exitCh:
			case <-time.After(probeReapGiveUp):
				log.Warn("probe reap timed out; abandoning child",
					"pid", proc.Pid, "give_up", probeReapGiveUp)
			}
			return ctx.Err()
		}
	}
}

// waitReady runs the probe in a loop until it returns nil or the total
// timeout elapses. Each probe attempt inherits ctx, so cancellation
// kills any in-flight probe immediately.
func waitReady(ctx context.Context, ready *config.Ready, env []string, cwd string, prober Prober, log *slog.Logger) error {
	deadline := time.Now().Add(ready.Timeout.Std())
	probeCtx, probeCancel := context.WithDeadline(ctx, deadline)
	defer probeCancel()

	// One reusable timer for the inter-probe interval. NewTimer + Stop
	// avoids the per-iteration allocation of time.After (whose Timer
	// keeps running even when the select picks a different case).
	intervalTimer := time.NewTimer(ready.Interval.Std())
	defer intervalTimer.Stop()

	for {
		if err := prober(probeCtx, ready.Command, env, cwd); err == nil {
			return nil
		} else {
			log.Debug("probe failed; retrying", "err", err)
		}
		// Reset for the next interval. Drain a stale tick if Stop
		// reports the timer had already fired (rare but possible).
		if !intervalTimer.Stop() {
			select {
			case <-intervalTimer.C:
			default:
			}
		}
		intervalTimer.Reset(ready.Interval.Std())
		select {
		case <-intervalTimer.C:
		case <-probeCtx.Done():
			if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("readiness timeout after %v", ready.Timeout.Std())
			}
			return probeCtx.Err()
		}
	}
}
