package reaper

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestReapNoChildren(t *testing.T) {
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
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
