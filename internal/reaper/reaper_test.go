package reaper

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReapNoChildren(t *testing.T) {
	r := New(discardLogger())
	done := make(chan struct{})
	go func() {
		r.Reap()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Reap blocked when no children were present")
	}
}

// drainReaper polls Reap from a goroutine to simulate the SIGCHLD-driven
// loop in main. Returned cleanup must be deferred.
func drainReaper(t *testing.T, r *Reaper) func() {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				r.Reap() // final drain
				return
			case <-ticker.C:
				r.Reap()
			}
		}
	}()
	return func() { close(stop); <-done }
}

func TestSpawnTracked_DispatchesExit(t *testing.T) {
	r := New(discardLogger())
	defer drainReaper(t, r)()

	for i := 0; i < 25; i++ {
		cmd := exec.Command("/bin/sh", "-c", ":")
		_, ch, err := r.SpawnTracked(func() (*os.Process, error) {
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return cmd.Process, nil
		})
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		select {
		case info := <-ch:
			if info.Signaled || info.ExitCode != 0 {
				t.Fatalf("iter %d: unexpected exit %+v", i, info)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: timeout waiting for exit", i)
		}
	}
}

func TestSpawnTracked_NonZeroExitCode(t *testing.T) {
	r := New(discardLogger())
	defer drainReaper(t, r)()

	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	_, ch, err := r.SpawnTracked(func() (*os.Process, error) {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case info := <-ch:
		if info.Signaled || info.ExitCode != 7 {
			t.Fatalf("got %+v, want exit 7", info)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestUntrack_FallsToOrphanLog(t *testing.T) {
	// Capture log output to verify orphan path.
	var mu sync.Mutex
	var captured []byte
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		captured = append(captured, p...)
		mu.Unlock()
		return len(p), nil
	})
	log := slog.New(slog.NewTextHandler(w, nil))

	r := New(log)
	defer drainReaper(t, r)()

	cmd := exec.Command("/bin/sh", "-c", "exit 3")
	proc, _, err := r.SpawnTracked(func() (*os.Process, error) {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Untrack(proc.Pid)

	// Wait for the reaper goroutine to log the orphan.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := string(captured)
		mu.Unlock()
		if len(got) > 0 {
			if !contains(got, "reaped orphan") {
				t.Fatalf("log does not mention orphan: %s", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no log output captured")
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
