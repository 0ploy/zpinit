package main

import (
	"context"
	"log/slog"
	"os"
	"syscall"
	"testing"
)

// fireOn is a slog handler that fires the latch when it sees msg: a
// stop signal landing in the middle of execCmd.
type fireOn struct {
	slog.Handler
	msg   string
	latch *stopLatch
}

func (fireOn) Enabled(context.Context, slog.Level) bool { return true }

func (h fireOn) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.latch.fire(syscall.SIGTERM)
	}
	return nil
}

func stubExecve(t *testing.T) *bool {
	t.Helper()
	called := new(bool)
	old := execve
	execve = func(string, []string, []string) error { *called = true; return nil }
	t.Cleanup(func() { execve = old })
	return called
}

// A stop that lands after the lookup (here: while the exec line is
// logged) must not be swallowed by the latch and lost across execve:
// the check runs immediately before the exec.
func TestExecCmdStopRightBeforeExec(t *testing.T) {
	called := stubExecve(t)
	latch := &stopLatch{done: make(chan struct{})}
	log := slog.New(fireOn{Handler: slog.DiscardHandler, msg: "exec", latch: latch})
	out := execCmd(log, []string{"sh", "-c", "true"}, map[string]string{"PATH": "/usr/bin:/bin"}, latch)
	if *called {
		t.Fatal("exec'd the CMD with a stop already latched; the stop is lost")
	}
	if out.Signal != syscall.SIGTERM || out.Code != 128+int(syscall.SIGTERM) {
		t.Errorf("outcome = %+v, want SIGTERM / 143", out)
	}
}

func TestExecCmdResolvesAgainstMergedPathWithoutSetenv(t *testing.T) {
	called := stubExecve(t)
	t.Setenv("PATH", "/nonexistent")
	latch := &stopLatch{done: make(chan struct{})}
	out := execCmd(slog.New(slog.DiscardHandler), []string{"sh"}, map[string]string{"PATH": "/usr/bin:/bin"}, latch)
	if !*called || out.Code != 0 {
		t.Fatalf("sh not resolved against the merged PATH: called=%v outcome=%+v", *called, out)
	}
	if got := os.Getenv("PATH"); got != "/nonexistent" {
		t.Errorf("zpinit's own PATH rewritten to %q", got)
	}
	out = execCmd(slog.New(slog.DiscardHandler), []string{"definitely-not-a-command-zpinit"}, map[string]string{"PATH": "/usr/bin:/bin"}, latch)
	if out.Code != 127 {
		t.Errorf("missing command: outcome = %+v, want 127", out)
	}
}
