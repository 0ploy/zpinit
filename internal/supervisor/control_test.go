package supervisor

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0ploy/zpinit/internal/config"
)

func TestReadLastBytes_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.log")
	if err := os.WriteFile(real, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if _, err := readLastBytes(link, 4096); err == nil {
		t.Fatal("readLastBytes(symlink): want error, got nil")
	}

	if got, err := readLastBytes(real, 4096); err != nil {
		t.Fatalf("readLastBytes(real): %v", err)
	} else if got != "hello\n" {
		t.Errorf("readLastBytes(real) = %q, want %q", got, "hello\n")
	}
}

func resolveTargetFixture(t *testing.T) []*Runner {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mkSvc := func(name string, replicas int) config.Service {
		return config.Service{Name: name, Filename: "10_" + name + ".toml", Replicas: config.Replicas{N: replicas}}
	}
	// Non-replicated api, three-replica consumer.
	var snap []*Runner
	snap = append(snap, expandServiceToRunners(mkSvc("api", 1), nil, nil, nil, log)...)
	snap = append(snap, expandServiceToRunners(mkSvc("consumer", 3), nil, nil, nil, log)...)
	return snap
}

func TestResolveTarget_SingleReplica(t *testing.T) {
	snap := resolveTargetFixture(t)
	rs, err := resolveTarget(snap, "api")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if len(rs) != 1 || rs[0].DisplayName() != "api" {
		t.Errorf("got %d runners, names=%v", len(rs), runnerNames(rs))
	}
}

func TestResolveTarget_BareNameReturnsAllReplicas(t *testing.T) {
	snap := resolveTargetFixture(t)
	rs, err := resolveTarget(snap, "consumer")
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	if len(rs) != 3 {
		t.Fatalf("got %d, want 3", len(rs))
	}
	want := []string{"consumer/0", "consumer/1", "consumer/2"}
	got := runnerNames(rs)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveTarget_SlashNSelectsOne(t *testing.T) {
	snap := resolveTargetFixture(t)
	rs, err := resolveTarget(snap, "consumer/2")
	if err != nil {
		t.Fatalf("consumer/2: %v", err)
	}
	if len(rs) != 1 || rs[0].DisplayName() != "consumer/2" {
		t.Errorf("got %v", runnerNames(rs))
	}
}

func TestResolveTarget_UnknownService(t *testing.T) {
	snap := resolveTargetFixture(t)
	if _, err := resolveTarget(snap, "ghost"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("expected unknown service error, got %v", err)
	}
}

func TestResolveTarget_ReplicaOutOfRange(t *testing.T) {
	snap := resolveTargetFixture(t)
	_, err := resolveTarget(snap, "consumer/9")
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("expected out-of-range error, got %v", err)
	}
}

func TestResolveTarget_BadIndex(t *testing.T) {
	snap := resolveTargetFixture(t)
	_, err := resolveTarget(snap, "consumer/abc")
	if err == nil || !strings.Contains(err.Error(), "invalid replica index") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func runnerNames(rs []*Runner) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.DisplayName()
	}
	return out
}

func TestReadLastBytes_RejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	if _, err := readLastBytes(dir, 4096); err == nil {
		t.Fatal("readLastBytes(directory): want error, got nil")
	} else if !strings.Contains(err.Error(), "regular") && !strings.Contains(err.Error(), "directory") {
		// Either our explicit check fired, or the OS rejected it
		// (some platforms reject directory reads earlier). Either is
		// acceptable; we just want it not to silently succeed.
		t.Logf("readLastBytes(directory) error: %v", err)
	}
}
