package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
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

// defaultProber spawns the probe command via the centralized reaper.
// stdout/stderr are discarded — probes are noisy and the result is
// already captured in the exit code.
func defaultProber(r *reaper.Reaper, log *slog.Logger) Prober {
	return func(ctx context.Context, cmd []string, env []string, cwd string) error {
		if len(cmd) == 0 {
			return errors.New("empty probe command")
		}
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Env = env
		c.Dir = cwd
		c.Stdin = nil
		c.Stdout = io.Discard
		c.Stderr = io.Discard
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
			<-exitCh // wait for reap
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

	for {
		if err := prober(probeCtx, ready.Command, env, cwd); err == nil {
			return nil
		} else {
			log.Debug("probe failed; retrying", "err", err)
		}
		select {
		case <-time.After(ready.Interval.Std()):
		case <-probeCtx.Done():
			if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("readiness timeout after %v", ready.Timeout.Std())
			}
			return probeCtx.Err()
		}
	}
}
