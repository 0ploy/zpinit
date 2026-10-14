//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Boot, readiness, crash-loop and exit_code_from semantics, driven
// through the real binary. These are the paths an orchestrator sees
// as "container could not start" / "container ended", so each one
// pins the exit code and the observable zpctl state, not just logs.

// waitForStateRow polls `zpctl status` until the named row shows the
// wanted supervisord-style state. Returns the last status output.
func waitForStateRow(zpctl func(args ...string) (string, int), name, want string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var out string
	for time.Now().Before(deadline) {
		out, _ = zpctl("status")
		if stateOfRow(out, name) == want {
			return out, true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return out, false
}

// waitFileContains polls path until it contains substr.
func waitFileContains(path, substr string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var body string
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			body = string(data)
			if strings.Contains(body, substr) {
				return body, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return body, false
}

// watchExit reaps zp in the background. The returned channel closes
// once zp has exited and *err then holds the Wait error; the cleanup
// SIGKILLs a still-running zp so a failed test never leaks it.
func watchExit(t *testing.T, zp *exec.Cmd) (<-chan struct{}, *error) {
	t.Helper()
	done := make(chan struct{})
	var err error
	go func() {
		err = zp.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = zp.Process.Signal(syscall.SIGKILL)
		<-done
	})
	return done, &err
}

func envFileEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{"ZPINIT_ENV_FILE": filepath.Join(t.TempDir(), "env")}
}

// A [ready] probe that never passes with on_timeout = "fail" is a boot
// failure: PID 1 exits 1 and the next service in filename order never
// starts.
func TestReadyProbeTimeoutFailsBoot(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "second-ran")
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"

[ready]
command = ["/bin/false"]
interval = "20ms"
timeout = "300ms"
on_timeout = "fail"
`)
	writeFile(t, filepath.Join(cfg, "services", "20_after.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "touch %s; sleep 30"]
restart = "always"
stop_timeout = "1s"
`, marker))

	res := runZpinit(t, envFileEnv(t), "--config", cfg)
	if res.exitCode != 1 {
		t.Errorf("exit = %d, want 1 (boot failure)\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("second service started even though the first never became ready\nstderr:\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "app") {
		t.Errorf("stderr should name the failing service:\n%s", res.stderr)
	}
}

// on_timeout = "continue" makes the probe advisory: boot proceeds, the
// service counts as ready for `zpctl ready`, and the container stays up.
func TestReadyProbeTimeoutContinue(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf("control_socket = %q\n", socket))
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"

[ready]
command = ["/bin/false"]
interval = "20ms"
timeout = "200ms"
on_timeout = "continue"
`)
	writeFile(t, filepath.Join(cfg, "services", "20_after.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"
`)
	zp, stderr := startZpinit(t, cfg, socket)
	defer stopZpinit(t, zp)
	zpctl := zpctlRunner(t, socket)

	if !waitForRunning(zpctl, "after", 3*time.Second) {
		out, _ := zpctl("status")
		t.Fatalf("second service never RUNNING after advisory probe timeout:\n%s\nstderr:\n%s", out, stderr.String())
	}
	if out, code := zpctl("ready"); code != 0 {
		t.Errorf("ready exit = %d, want 0 (on_timeout=continue counts as ready):\n%s", code, out)
	}
}

// A [ready] probe that passes late still gates the next service: the
// second service's start is observed only after the probe's condition
// is met.
func TestReadyProbeGatesNextService(t *testing.T) {
	cfg := t.TempDir()
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	marker := filepath.Join(dir, "second-ran")
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "sleep 0.3; touch %s; sleep 30"]
restart = "always"
stop_timeout = "1s"

[ready]
command = ["/usr/bin/test", "-e", %q]
interval = "20ms"
timeout = "5s"
`, gate, gate))
	writeFile(t, filepath.Join(cfg, "services", "20_after.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "touch %s; sleep 30"]
restart = "always"
stop_timeout = "1s"
`, marker))
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf("control_socket = %q\n", socket))

	zp, stderr := startZpinit(t, cfg, socket)
	defer stopZpinit(t, zp)
	zpctl := zpctlRunner(t, socket)
	if !waitForRunning(zpctl, "after", 5*time.Second) {
		t.Fatalf("second service never ran\nstderr:\n%s", stderr.String())
	}
	gi, err := os.Stat(gate)
	if err != nil {
		t.Fatalf("gate file missing: %v", err)
	}
	mi, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("marker file missing: %v", err)
	}
	if mi.ModTime().Before(gi.ModTime()) {
		t.Errorf("second service started (%v) before the first became ready (%v)", mi.ModTime(), gi.ModTime())
	}
}

// Default on_boot_failure = "fail": a service that dies during initial
// boot under restart = "never" aborts PID 1 with exit 1.
//
// Without a [ready] probe a service is "booted" the moment it is
// spawned, so an immediate exit is a normal EXITED, not a boot failure.
// Dying while the probe is still polling is the boot-failure path.
func TestBootFailureAbortsContainer(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "second-ran")
	writeFile(t, filepath.Join(cfg, "services", "10_broken.toml"), `
command = ["/bin/sh", "-c", "sleep 0.1; exit 3"]
restart = "never"

[ready]
command = ["/bin/false"]
interval = "20ms"
timeout = "5s"
`)
	writeFile(t, filepath.Join(cfg, "services", "20_after.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "touch %s; sleep 30"]
restart = "always"
stop_timeout = "1s"
`, marker))
	res := runZpinit(t, envFileEnv(t), "--config", cfg)
	if res.exitCode != 1 {
		t.Errorf("exit = %d, want 1\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("later service booted after a fatal boot failure")
	}
}

// A command that cannot be exec'd at all is the other boot-failure
// shape. Spawn errors are retried through the crash backoff regardless
// of restart policy (restart = "never" only governs exits of a child
// that did start), so boot fails once the budget is exhausted and the
// service is FATAL; with default backoff that is ~30s, here it is
// milliseconds.
func TestBootFailureSpawnError(t *testing.T) {
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "services", "10_broken.toml"), `
command = ["/nonexistent/binary"]
restart = "never"
backoff_initial = "10ms"
backoff_max = "20ms"
`)
	start := time.Now()
	res := runZpinit(t, envFileEnv(t), "--config", cfg)
	if res.exitCode != 1 {
		t.Errorf("exit = %d, want 1\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("boot failure took %v to surface", el)
	}
	if !strings.Contains(res.stderr, "spawn failed") {
		t.Errorf("stderr should report the spawn failure:\n%s", res.stderr)
	}
}

// on_boot_failure = "continue" keeps the container up: the broken
// service is visible in status (not RUNNING), the services behind it
// boot, and `zpctl ready` reports the broken one.
func TestOnBootFailureContinue(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf("control_socket = %q\n", socket))
	writeFile(t, filepath.Join(cfg, "services", "10_broken.toml"), `
command = ["/bin/sh", "-c", "sleep 0.1; exit 3"]
restart = "never"
on_boot_failure = "continue"

[ready]
command = ["/bin/false"]
interval = "20ms"
timeout = "300ms"
`)
	writeFile(t, filepath.Join(cfg, "services", "20_after.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"
`)
	zp, stderr := startZpinit(t, cfg, socket)
	defer stopZpinit(t, zp)
	zpctl := zpctlRunner(t, socket)

	if !waitForRunning(zpctl, "after", 3*time.Second) {
		t.Fatalf("service behind the broken one never ran\nstderr:\n%s", stderr.String())
	}
	out, _ := zpctl("status")
	if st := stateOfRow(out, "broken"); st != "EXITED" {
		t.Errorf("broken state = %q, want EXITED:\n%s", st, out)
	}
	out, code := zpctl("ready")
	if code != 1 || !strings.Contains(out, "broken") {
		t.Errorf("ready exit = %d, want 1 naming broken:\n%s", code, out)
	}
	out, code = zpctl("ready", "after")
	if code != 0 {
		t.Errorf("ready after exit = %d, want 0:\n%s", code, out)
	}
}

// exit_code_from: a restart = "never" worker ending on its own ends the
// container with the worker's exit code, even while a sidecar keeps
// running.
func TestExitCodeFromPropagatesWorkerExit(t *testing.T) {
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), `exit_code_from = "worker"`+"\n")
	writeFile(t, filepath.Join(cfg, "services", "10_side.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"
`)
	writeFile(t, filepath.Join(cfg, "services", "20_worker.toml"), `
command = ["/bin/sh", "-c", "sleep 0.2; exit 7"]
restart = "never"
`)
	start := time.Now()
	res := runZpinit(t, envFileEnv(t), "--config", cfg)
	if res.exitCode != 7 {
		t.Errorf("exit = %d, want 7 (worker's code)\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("container took %v to end after worker exit", el)
	}
}

// exit_code_from under a restarting policy: transient crashes are
// retried, but crash-looping to FATAL ends the container non-zero.
func TestExitCodeFromFatalAfterCrashLoop(t *testing.T) {
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), `exit_code_from = "worker"`+"\n")
	writeFile(t, filepath.Join(cfg, "services", "10_worker.toml"), `
command = ["/bin/sh", "-c", "exit 5"]
restart = "always"
backoff_initial = "10ms"
backoff_max = "20ms"
on_boot_failure = "continue"
`)
	res := runZpinit(t, envFileEnv(t), "--config", cfg)
	if res.exitCode != 5 {
		t.Errorf("exit = %d, want 5 (last crash's code)\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(strings.ToLower(res.stderr), "fatal") {
		t.Errorf("stderr should mention FATAL:\n%s", res.stderr)
	}
}

// Operator actions on the exit_code_from target never end the
// container: `zpctl restart` and `zpctl stop` leave PID 1 running;
// only `zpctl shutdown` ends it.
func TestExitCodeFromSurvivesOperatorActions(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf("control_socket = %q\nexit_code_from = \"worker\"\n", socket))
	writeFile(t, filepath.Join(cfg, "services", "10_worker.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "never"
stop_timeout = "1s"
`)
	zp, stderr := startZpinit(t, cfg, socket)
	exited, exitErr := watchExit(t, zp)
	zpctl := zpctlRunner(t, socket)

	pidOut, _ := zpctl("pid", "worker")
	if out, code := zpctl("restart", "--wait", "worker"); code != 0 {
		t.Fatalf("restart exit %d:\n%s", code, out)
	}
	if !waitForRunningPIDChange(zpctl, "worker", strings.TrimSpace(pidOut), 3*time.Second) {
		t.Fatalf("worker PID did not change after restart\nstderr:\n%s", stderr.String())
	}
	if out, code := zpctl("stop", "worker"); code != 0 {
		t.Fatalf("stop exit %d:\n%s", code, out)
	}
	if _, ok := waitForStateRow(zpctl, "worker", "STOPPED", 3*time.Second); !ok {
		t.Fatalf("worker never STOPPED")
	}
	select {
	case <-exited:
		t.Fatalf("zpinit exited after operator restart/stop of the exit_code_from target: %v\nstderr:\n%s", *exitErr, stderr.String())
	case <-time.After(500 * time.Millisecond):
	}

	if out, code := zpctl("shutdown"); code != 0 {
		t.Fatalf("shutdown exit %d:\n%s", code, out)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatalf("zpinit did not exit after zpctl shutdown\nstderr:\n%s", stderr.String())
	}
	// With exit_code_from set, every shutdown path (watcher, signal,
	// zpctl shutdown) reports the target's LAST exit, and the operator
	// stop above ended it with SIGTERM: 128+15. Pinned so a change to
	// that rule is deliberate, not accidental.
	if code := exitCodeOf(*exitErr); code != 143 {
		t.Errorf("zpinit exit after shutdown = %d, want 143 (target's last exit, SIGTERM)\nstderr:\n%s", code, stderr.String())
	}
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// A service that crashes MaxConsecutiveCrashes times goes FATAL and
// stays there; the container keeps running without exit_code_from.
// Pins the verbose and JSON status renderings of that state, and that
// `zpctl start` resets the crash budget.
func TestCrashLoopGoesFatal(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf("control_socket = %q\n", socket))
	writeFile(t, filepath.Join(cfg, "services", "10_crash.toml"), `
command = ["/bin/sh", "-c", "exit 1"]
restart = "always"
backoff_initial = "10ms"
backoff_max = "20ms"
on_boot_failure = "continue"
`)
	zp, stderr := startZpinit(t, cfg, socket)
	defer stopZpinit(t, zp)
	zpctl := zpctlRunner(t, socket)

	out, ok := waitForStateRow(zpctl, "crash", "FATAL", 5*time.Second)
	if !ok {
		t.Fatalf("service never reached FATAL:\n%s\nstderr:\n%s", out, stderr.String())
	}
	out, _ = zpctl("status", "--verbose", "crash")
	if !strings.Contains(out, "spawns=5") || !strings.Contains(out, "last_exit=code:1") {
		t.Errorf("verbose status missing crash budget / last exit:\n%s", out)
	}
	out, _ = zpctl("status", "--json", "crash")
	for _, want := range []string{`"name":"crash"`, `"state":"FATAL"`, `"total_spawns":5`, `"code":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("json status missing %s:\n%s", want, out)
		}
	}
	out, code := zpctl("ready")
	if code != 1 || !strings.Contains(out, "crash: fatal") {
		t.Errorf("ready exit = %d, want 1 with 'crash: fatal':\n%s", code, out)
	}
	// Operator start resets the budget: the service spawns again.
	if out, code := zpctl("start", "crash"); code != 0 {
		t.Fatalf("start exit %d:\n%s", code, out)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, _ = zpctl("status", "--verbose", "crash")
		if strings.Contains(out, "spawns=6") || strings.Contains(out, "spawns=7") {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !strings.Contains(out, "spawns=6") && !strings.Contains(out, "spawns=7") {
		t.Errorf("start did not respawn the FATAL service:\n%s", out)
	}
}
