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

// TestReloadSig: a service with reload_signal = "USR1"
// keeps its PID across `zpctl reload`. The wrapped /bin/sh handles
// USR1 by writing a marker, which lets us verify the signal arrived
// without bringing in a daemon-style fixture.
func TestReloadSig(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	marker := filepath.Join(t.TempDir(), "usr1.marker")

	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
`, socket))
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "trap 'echo got-usr1 >> %s' USR1; while true; do sleep 1; done"]
reload_signal = "USR1"
restart = "always"
stop_timeout = "1s"
`, marker))

	zp, zpStderr := startZpinit(t, cfg, socket)
	defer stopZpinit(t, zp)

	zpctl := zpctlRunner(t, socket)

	if !waitForRunning(zpctl, "app", 3*time.Second) {
		t.Fatalf("app not RUNNING; stderr:\n%s", zpStderr.String())
	}

	pidBefore, code := zpctl("pid", "app")
	if code != 0 {
		t.Fatalf("pid before exit %d: %s", code, pidBefore)
	}
	pidBefore = strings.TrimSpace(pidBefore)

	out, code := zpctl("reload", "app")
	if code != 0 {
		t.Fatalf("reload app exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "app: reloaded") {
		t.Errorf("expected 'app: reloaded' in output, got:\n%s", out)
	}

	// Wait for the trap handler to write the marker.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil && strings.Contains(string(data), "got-usr1") {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	data, _ := os.ReadFile(marker)
	if !strings.Contains(string(data), "got-usr1") {
		t.Errorf("USR1 trap never fired; marker=%q", string(data))
	}

	pidAfter, code := zpctl("pid", "app")
	if code != 0 {
		t.Fatalf("pid after exit %d: %s", code, pidAfter)
	}
	pidAfter = strings.TrimSpace(pidAfter)
	if pidBefore != pidAfter {
		t.Errorf("PID changed across signal-reload: before=%s after=%s", pidBefore, pidAfter)
	}
}

// TestReloadFb: a service without reload_signal or
// reload_command falls back to stop+start; the PID must change.
func TestReloadFb(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")

	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
`, socket))
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sh", "-c", "while true; do sleep 1; done"]
restart = "always"
stop_timeout = "2s"
`)

	zp, zpStderr := startZpinit(t, cfg, socket)
	defer stopZpinit(t, zp)
	zpctl := zpctlRunner(t, socket)

	if !waitForRunning(zpctl, "app", 3*time.Second) {
		t.Fatalf("app not RUNNING; stderr:\n%s", zpStderr.String())
	}
	pidBefore, _ := zpctl("pid", "app")
	pidBefore = strings.TrimSpace(pidBefore)

	out, code := zpctl("reload", "app")
	if code != 0 {
		t.Fatalf("reload app exit %d:\n%s", code, out)
	}

	// Fallback path stops then starts; give the runner a moment to
	// finish the cycle.
	if !waitForRunningPIDChange(zpctl, "app", pidBefore, 5*time.Second) {
		t.Errorf("PID did not change after fallback reload; before=%s", pidBefore)
	}
}

// TestReloadCmd: reload_command runs as a one-shot when
// `zpctl reload <name>` is invoked. The command appends a line to a
// marker file; the live service's PID stays the same.
func TestReloadCmd(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	marker := filepath.Join(t.TempDir(), "cmd.marker")

	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
`, socket))
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "while true; do sleep 1; done"]
reload_command = ["/bin/sh", "-c", "echo cmd-ran >> %s"]
restart = "always"
stop_timeout = "1s"
`, marker))

	zp, zpStderr := startZpinit(t, cfg, socket)
	defer stopZpinit(t, zp)
	zpctl := zpctlRunner(t, socket)

	if !waitForRunning(zpctl, "app", 3*time.Second) {
		t.Fatalf("app not RUNNING; stderr:\n%s", zpStderr.String())
	}
	pidBefore, _ := zpctl("pid", "app")
	pidBefore = strings.TrimSpace(pidBefore)

	out, code := zpctl("reload", "app")
	if code != 0 {
		t.Fatalf("reload app exit %d:\n%s", code, out)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil && strings.Contains(string(data), "cmd-ran") {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	data, _ := os.ReadFile(marker)
	if !strings.Contains(string(data), "cmd-ran") {
		t.Fatalf("reload_command never wrote marker; content=%q", string(data))
	}

	pidAfter, _ := zpctl("pid", "app")
	if strings.TrimSpace(pidAfter) != pidBefore {
		t.Errorf("PID changed across reload_command: before=%s after=%s (reload_command should not restart the service)", pidBefore, strings.TrimSpace(pidAfter))
	}
}

// TestReloadOnCPU: zpinit watches the cgroup fixture for CPU
// changes; when the operator rewrites cpu.max the service with
// reload_on_change = ["cpu"] gets reloaded (here: full restart, so
// the PID changes). Uses very short scale_up_after so the test
// doesn't drag in wall-clock time.
func TestReloadOnCPU(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	cg := t.TempDir()
	proc := t.TempDir()
	// 2 CPUs initially.
	writeFile(t, filepath.Join(cg, "cpu.max"), "200000 100000\n")
	writeFile(t, filepath.Join(cg, "memory.max"), "1073741824\n")
	writeFile(t, filepath.Join(proc, "cpuinfo"),
		"processor\t: 0\nprocessor\t: 1\nprocessor\t: 2\nprocessor\t: 3\n")
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       16777216 kB\n")

	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
[resources]
scale_up_after   = "200ms"
scale_down_after = "200ms"
`, socket))
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sh", "-c", "while true; do sleep 1; done"]
restart = "always"
stop_timeout = "1s"
reload_on_change = ["cpu"]
`)

	zp, zpStderr := startZpinitWithEnv(t, cfg, socket, map[string]string{
		"ZPINIT_CGROUP_ROOT": cg,
		"ZPINIT_PROC_ROOT":   proc,
	})
	defer stopZpinit(t, zp)
	zpctl := zpctlRunner(t, socket)

	if !waitForRunning(zpctl, "app", 3*time.Second) {
		t.Fatalf("app not RUNNING; stderr:\n%s", zpStderr.String())
	}
	pidBefore, _ := zpctl("pid", "app")
	pidBefore = strings.TrimSpace(pidBefore)

	// Bump cgroup to 4 CPUs.
	writeFile(t, filepath.Join(cg, "cpu.max"), "400000 100000\n")

	if !waitForRunningPIDChange(zpctl, "app", pidBefore, 3*time.Second) {
		t.Errorf("app PID did not change after cpu-change reload trigger; stderr tail:\n%s", zpStderr.String())
	}
}

// TestReloadOnMemoryFiltered: a service with reload_on_change =
// ["memory"] is NOT triggered by a cpu change. Same scaffolding as
// TestReloadOnCPU; just verifies the dimension filter.
func TestReloadOnMemoryFiltered(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	cg := t.TempDir()
	proc := t.TempDir()
	writeFile(t, filepath.Join(cg, "cpu.max"), "200000 100000\n")
	writeFile(t, filepath.Join(cg, "memory.max"), "1073741824\n")
	writeFile(t, filepath.Join(proc, "cpuinfo"),
		"processor\t: 0\nprocessor\t: 1\nprocessor\t: 2\nprocessor\t: 3\n")
	writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       16777216 kB\n")

	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
[resources]
scale_up_after   = "100ms"
scale_down_after = "100ms"
`, socket))
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sh", "-c", "while true; do sleep 1; done"]
restart = "always"
stop_timeout = "1s"
reload_on_change = ["memory"]
`)

	zp, _ := startZpinitWithEnv(t, cfg, socket, map[string]string{
		"ZPINIT_CGROUP_ROOT": cg,
		"ZPINIT_PROC_ROOT":   proc,
	})
	defer stopZpinit(t, zp)
	zpctl := zpctlRunner(t, socket)

	if !waitForRunning(zpctl, "app", 3*time.Second) {
		t.Fatal("app not RUNNING")
	}
	pidBefore, _ := zpctl("pid", "app")
	pidBefore = strings.TrimSpace(pidBefore)

	// Cpu change only — memory-only listener must NOT restart.
	writeFile(t, filepath.Join(cg, "cpu.max"), "400000 100000\n")

	// Allow enough time for the watcher to commit (100 ms scale_up_after
	// + poll interval ~1 s). If the filter is broken the runner would
	// restart in that window.
	time.Sleep(1500 * time.Millisecond)
	pidAfter, _ := zpctl("pid", "app")
	if strings.TrimSpace(pidAfter) != pidBefore {
		t.Errorf("PID changed despite reload_on_change=[memory]; before=%s after=%s", pidBefore, strings.TrimSpace(pidAfter))
	}
}

// startZpinit, stopZpinit, zpctlRunner, waitForRunning, and
// waitForRunningPIDChange are local helpers reused across the three
// reload tests above. Kept beside the tests rather than in the
// shared integration_test.go file so they don't accidentally become
// part of the broader contract.

func startZpinit(t *testing.T, cfg, socket string) (*exec.Cmd, *bytes.Buffer) {
	return startZpinitWithEnv(t, cfg, socket, nil)
}

func startZpinitWithEnv(t *testing.T, cfg, socket string, extra map[string]string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	envFile := filepath.Join(t.TempDir(), "env")
	zp := exec.Command(zpinitBin, "--config", cfg)
	env := append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	zp.Env = env
	var stderr bytes.Buffer
	zp.Stderr = &stderr
	if err := zp.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitForSocket(t, socket, 3*time.Second) {
		_ = zp.Process.Signal(syscall.SIGTERM)
		_ = zp.Wait()
		t.Fatalf("control socket never appeared; stderr:\n%s", stderr.String())
	}
	return zp, &stderr
}

func stopZpinit(t *testing.T, zp *exec.Cmd) {
	t.Helper()
	_ = zp.Process.Signal(syscall.SIGTERM)
	_ = zp.Wait()
}

func zpctlRunner(t *testing.T, socket string) func(args ...string) (string, int) {
	t.Helper()
	return func(args ...string) (string, int) {
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
}

func waitForRunning(zpctl func(args ...string) (string, int), name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := zpctl("status")
		if stateOfRow(out, name) == "RUNNING" {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false
}

func waitForRunningPIDChange(zpctl func(args ...string) (string, int), name, oldPID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := zpctl("pid", name)
		newPID := strings.TrimSpace(out)
		if newPID != "" && newPID != oldPID {
			// Also wait for it to be RUNNING again so subsequent
			// tests don't race with the restart's transient states.
			if waitForRunning(zpctl, name, time.Second) {
				return true
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false
}
