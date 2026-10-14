//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The rest of this suite runs zpinit as an ordinary process, which
// cannot observe the two things that only exist for PID 1: orphaned
// grandchildren being reparented to zpinit (and reaped there), and the
// container ending with zpinit's exit code. This test runs the real
// binary as PID 1 in a throwaway container. It needs a docker CLI with
// a reachable daemon and skips otherwise: the dev container has none,
// the ubuntu CI runner does.
func TestPID1InContainer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("docker PID 1 test is Linux-only")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v\n%s", err, out)
	}

	// Static binaries: the TestMain ones may be cgo-linked against the
	// host's glibc, which an alpine (musl) container cannot load.
	dir := t.TempDir()
	bins := map[string]string{}
	for _, name := range []string{"zpinit", "zpctl"} {
		path := filepath.Join(dir, name)
		build := exec.Command("go", "build", "-o", path, "../../cmd/"+name)
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, out)
		}
		bins[name] = path
	}

	// app double-forks a short-lived grandchild: the subshell forks
	// `sleep`, exits, and the sleep is reparented to PID 1. When it ends
	// half a second later only PID 1 can reap it.
	cfg := filepath.Join(dir, "etc")
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sh", "-c", "(sleep 0.5 &); exec sleep 60"]
restart = "always"
stop_timeout = "3s"
`)
	if err := os.Chmod(cfg, 0o755); err != nil {
		t.Fatal(err)
	}

	name := fmt.Sprintf("zpinit-it-%d-%d", os.Getpid(), time.Now().UnixNano())
	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = docker("rm", "-f", name) })

	if out, err := docker("run", "-d", "--name", name,
		"-v", bins["zpinit"]+":/usr/local/bin/zpinit:ro",
		"-v", bins["zpctl"]+":/usr/local/bin/zpctl:ro",
		"-v", cfg+":/etc/zpinit:ro",
		"alpine:3.20", "zpinit"); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	logs := func() string {
		out, _ := docker("logs", name)
		return out
	}

	// zpinit really is PID 1 in there, and the control socket works.
	deadline := time.Now().Add(15 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		out, err := docker("exec", name, "zpctl", "status")
		status = out
		if err == nil && strings.Contains(out, "RUNNING") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(status, "RUNNING") {
		t.Fatalf("app never RUNNING inside the container; status:\n%s\nlogs:\n%s", status, logs())
	}
	if out, err := docker("exec", name, "cat", "/proc/1/comm"); err != nil || strings.TrimSpace(out) != "zpinit" {
		t.Errorf("/proc/1/comm = %q (%v), want zpinit", strings.TrimSpace(out), err)
	}

	// The orphaned grandchild gets reaped by zpinit, not left a zombie.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs(), "reaped orphan") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(logs(), "reaped orphan") {
		t.Errorf("orphaned grandchild was never reaped; logs:\n%s", logs())
	}
	if out, _ := docker("exec", name, "sh", "-c", "grep -l '^State:.Z' /proc/[0-9]*/status 2>/dev/null | wc -l"); strings.TrimSpace(out) != "0" {
		t.Errorf("zombies left in the container: %s", strings.TrimSpace(out))
	}

	// docker stop (SIGTERM to PID 1) is a clean exit 0 inside the stop
	// budget, which is what an orchestrator keys on.
	start := time.Now()
	if out, err := docker("stop", "-t", "10", name); err != nil {
		t.Fatalf("docker stop: %v\n%s", err, out)
	}
	if el := time.Since(start); el > 8*time.Second {
		t.Errorf("docker stop took %v; graceful shutdown should be well inside the 10s budget", el)
	}
	out, err := docker("inspect", "-f", "{{.State.ExitCode}}", name)
	if err != nil || strings.TrimSpace(out) != "0" {
		t.Errorf("container exit code = %q (%v), want 0; logs:\n%s", strings.TrimSpace(out), err, logs())
	}
}
