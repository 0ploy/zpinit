//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReplicas_SpawnsNCopies: a service declared with replicas = 3
// produces three independent supervised children. Each one shows up
// as its own row in `zpctl status` with a distinct PID, and each one
// can be targeted individually via svc/N.
func TestReplicas_SpawnsNCopies(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
`, socket))
	logDir := t.TempDir()
	writeFile(t, filepath.Join(cfg, "services", "10_worker.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "echo replica $ZPINIT_REPLICA_INDEX; sleep 30"]
replicas = 3
restart = "always"
stop_timeout = "1s"
[log]
stdout = "%s/worker.log"
`, logDir))

	envFile := filepath.Join(t.TempDir(), "env")
	zp := exec.Command(zpinitBin, "--config", cfg)
	zp.Env = append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	var zpStderr bytes.Buffer
	zp.Stderr = &zpStderr
	if err := zp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// SIGTERM (not SIGKILL) so the supervisor's stopAll fans out to
		// the children before exit; otherwise grandchildren keep
		// inheriting the test's stderr pipe and the cmd.Wait() in this
		// cleanup blocks until the children's own sleep finishes.
		_ = zp.Process.Signal(syscall.SIGTERM)
		_ = zp.Wait()
	})

	if !waitForSocket(t, socket, 3*time.Second) {
		t.Fatalf("control socket never appeared; stderr:\n%s", zpStderr.String())
	}

	zpctl := func(args ...string) (string, int) {
		t.Helper()
		full := append([]string{"--socket", socket}, args...)
		c := exec.Command(zpctlBin, full...)
		var out, errBuf bytes.Buffer
		c.Stdout = &out
		c.Stderr = &errBuf
		err := c.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("zpctl run: %v\nstderr:\n%s", err, errBuf.String())
		}
		return out.String() + errBuf.String(), code
	}

	// status should show three rows: worker/0, worker/1, worker/2.
	var out string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, _ = zpctl("status")
		if strings.Contains(out, "worker/0") && strings.Contains(out, "worker/1") && strings.Contains(out, "worker/2") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		needle := fmt.Sprintf("worker/%d", i)
		if !strings.Contains(out, needle) {
			t.Errorf("status missing %s:\n%s", needle, out)
		}
	}
	if !strings.Contains(out, "RUNNING") {
		t.Errorf("status: no RUNNING rows:\n%s", out)
	}

	// Each replica's pid command works.
	var pids [3]string
	for i := 0; i < 3; i++ {
		out, code := zpctl("pid", fmt.Sprintf("worker/%d", i))
		if code != 0 {
			t.Fatalf("pid worker/%d exit %d:\n%s", i, code, out)
		}
		pids[i] = strings.TrimSpace(out)
	}
	if pids[0] == "" || pids[0] == pids[1] || pids[1] == pids[2] {
		t.Errorf("expected distinct PIDs, got %v", pids)
	}

	// Bare "worker" must refuse pid because it's ambiguous.
	out, code := zpctl("pid", "worker")
	if code == 0 {
		t.Errorf("zpctl pid worker should fail for replicated service; got:\n%s", out)
	}

	// stop worker/1; only the middle replica must end.
	out, code = zpctl("stop", "worker/1")
	if code != 0 {
		t.Fatalf("stop worker/1 exit %d:\n%s", code, out)
	}
	// Confirm worker/1 reaches a non-running state (STOPPED/EXITED) but
	// worker/0 and worker/2 stay RUNNING. restart=always plus the
	// stop_timeout means the runner records the stop, the process group
	// dies, and the runner remains in stop intent until restarted.
	deadline = time.Now().Add(2 * time.Second)
	var statusOut string
	for time.Now().Before(deadline) {
		statusOut, _ = zpctl("status")
		w1Stopped := stateOfRow(statusOut, "worker/1") == "STOPPED"
		if w1Stopped {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := stateOfRow(statusOut, "worker/1"); got != "STOPPED" {
		t.Errorf("worker/1 state = %s; want STOPPED. Full status:\n%s", got, statusOut)
	}
	if got := stateOfRow(statusOut, "worker/0"); got != "RUNNING" {
		t.Errorf("worker/0 state = %s; want RUNNING (sibling stop must not cascade). Full status:\n%s", got, statusOut)
	}
	if got := stateOfRow(statusOut, "worker/2"); got != "RUNNING" {
		t.Errorf("worker/2 state = %s; want RUNNING (sibling stop must not cascade). Full status:\n%s", got, statusOut)
	}

	// shutdown ends the supervisor.
	_, _ = zpctl("shutdown")
	if err := zp.Wait(); err != nil {
		t.Fatalf("zpinit didn't exit cleanly: %v\nstderr:\n%s", err, zpStderr.String())
	}
}

// TestReplicas_LogShared: without the {index} placeholder, every
// replica writes to the same file. Lines from different replicas land
// in the same file under O_APPEND. (Test name is intentionally short:
// macOS caps Unix-socket paths at 104 bytes, and t.TempDir() embeds
// the test name into the path.)
func TestReplicas_LogShared(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
`, socket))
	logDir := t.TempDir()
	shared := filepath.Join(logDir, "logger.log")
	writeFile(t, filepath.Join(cfg, "services", "10_logger.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "echo hello-from-$ZPINIT_REPLICA_INDEX; sleep 30"]
replicas = 2
restart = "always"
stop_timeout = "1s"
[log]
stdout = "%s"
`, shared))

	envFile := filepath.Join(t.TempDir(), "env")
	zp := exec.Command(zpinitBin, "--config", cfg)
	zp.Env = append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	var zpStderr bytes.Buffer
	zp.Stderr = &zpStderr
	if err := zp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = zp.Process.Signal(syscall.SIGTERM)
		_ = zp.Wait()
	})
	if !waitForSocket(t, socket, 3*time.Second) {
		t.Fatalf("control socket never appeared; stderr:\n%s", zpStderr.String())
	}

	// Both replicas should land lines in the same file.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(shared)
		if err == nil && strings.Contains(string(data), "hello-from-0") && strings.Contains(string(data), "hello-from-1") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, err := os.ReadFile(shared)
	if err != nil {
		t.Fatalf("read shared log: %v", err)
	}
	if !strings.Contains(string(data), "hello-from-0") {
		t.Errorf("shared log missing replica-0 marker; content=%q", string(data))
	}
	if !strings.Contains(string(data), "hello-from-1") {
		t.Errorf("shared log missing replica-1 marker; content=%q", string(data))
	}

	// No per-replica files (.0.log, .1.log) should have appeared.
	if _, err := os.Stat(filepath.Join(logDir, "logger.0.log")); err == nil {
		t.Error("logger.0.log should not exist under shared-default semantics")
	}
}

// TestReplicas_LogIndex: with the {index} placeholder, each replica
// writes to its own file and the supervisor must not cross-contaminate
// streams.
func TestReplicas_LogIndex(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
`, socket))
	logDir := t.TempDir()
	writeFile(t, filepath.Join(cfg, "services", "10_logger.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "echo hello-from-$ZPINIT_REPLICA_INDEX; sleep 30"]
replicas = 2
restart = "always"
stop_timeout = "1s"
[log]
stdout = "%s/logger-{index}.log"
`, logDir))

	envFile := filepath.Join(t.TempDir(), "env")
	zp := exec.Command(zpinitBin, "--config", cfg)
	zp.Env = append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	var zpStderr bytes.Buffer
	zp.Stderr = &zpStderr
	if err := zp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = zp.Process.Signal(syscall.SIGTERM)
		_ = zp.Wait()
	})
	if !waitForSocket(t, socket, 3*time.Second) {
		t.Fatalf("control socket never appeared; stderr:\n%s", zpStderr.String())
	}

	want := []struct {
		path    string
		content string
	}{
		{filepath.Join(logDir, "logger-0.log"), "hello-from-0"},
		{filepath.Join(logDir, "logger-1.log"), "hello-from-1"},
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		allFound := true
		for _, w := range want {
			data, err := os.ReadFile(w.path)
			if err != nil || !strings.Contains(string(data), w.content) {
				allFound = false
				break
			}
		}
		if allFound {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, w := range want {
		data, err := os.ReadFile(w.path)
		if err != nil {
			t.Errorf("read %s: %v", w.path, err)
			continue
		}
		if !strings.Contains(string(data), w.content) {
			t.Errorf("%s missing %q; content=%q", w.path, w.content, string(data))
		}
	}

	// Cross-contamination check: replica 0's log must NOT contain
	// replica 1's marker.
	data0, _ := os.ReadFile(want[0].path)
	if strings.Contains(string(data0), "hello-from-1") {
		t.Errorf("logger-0.log leaked replica-1 output: %q", string(data0))
	}
}

func waitForSocket(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false
}

// stateOfRow looks up the supervisord-style state column for the
// given service display name in the multi-line zpctl-status output.
// Returns "" when no row matches.
func stateOfRow(status, name string) string {
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == name {
			return fields[1]
		}
	}
	return ""
}
