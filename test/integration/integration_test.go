//go:build integration

package integration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	zpinitBin string
	zpctlBin  string
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "zpinit-it-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	for _, target := range []struct {
		name string
		bin  *string
	}{
		{"zpinit", &zpinitBin},
		{"zpctl", &zpctlBin},
	} {
		path := filepath.Join(tmp, target.name)
		build := exec.Command("go", "build", "-o", path, "../../cmd/"+target.name)
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: build %s: %v\n", target.name, err)
			os.Exit(1)
		}
		*target.bin = path
	}

	os.Exit(m.Run())
}

type runResult struct {
	stdout, stderr string
	exitCode       int
}

func runZpinit(t *testing.T, env map[string]string, args ...string) runResult {
	t.Helper()
	return runZpinitTimeout(t, env, 10*time.Second, args...)
}

func runZpinitTimeout(t *testing.T, env map[string]string, timeout time.Duration, args ...string) runResult {
	t.Helper()
	cmd := exec.Command(zpinitBin, args...)
	if env != nil {
		envv := os.Environ()
		for k, v := range env {
			envv = append(envv, k+"="+v)
		}
		cmd.Env = envv
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run: %v", err)
		}
		return runResult{stdout.String(), stderr.String(), code}
	case <-time.After(timeout):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-done
		t.Fatalf("zpinit did not exit within %v\nstdout:\n%s\nstderr:\n%s", timeout, stdout.String(), stderr.String())
		return runResult{}
	}
}

func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// #13: entrypoint.d runs scripts in lexicographic order.
func TestEntrypointSequential(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "log")

	writeExec(t, filepath.Join(cfg, "entrypoint.d", "10-first.sh"),
		"#!/bin/sh\necho first >> "+marker+"\n")
	writeExec(t, filepath.Join(cfg, "entrypoint.d", "20-second.sh"),
		"#!/bin/sh\necho second >> "+marker+"\n")
	writeExec(t, filepath.Join(cfg, "entrypoint.d", "30-third.sh"),
		"#!/bin/sh\necho third >> "+marker+"\n")

	res := runZpinit(t, map[string]string{
		"ZPINIT_ENV_FILE": filepath.Join(t.TempDir(), "env"),
	}, "--config", cfg, "--", "true")
	if res.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.exitCode, res.stderr)
	}

	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	want := "first\nsecond\nthird\n"
	if string(body) != want {
		t.Errorf("order = %q, want %q", string(body), want)
	}
}

// #14: entrypoint.d failure with default OnFailure=fail aborts boot;
// later scripts and CMD do not run.
func TestEntrypointFailureAborts(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "log")

	writeExec(t, filepath.Join(cfg, "entrypoint.d", "10-ok.sh"),
		"#!/bin/sh\necho a >> "+marker+"\nexit 0\n")
	writeExec(t, filepath.Join(cfg, "entrypoint.d", "20-fail.sh"),
		"#!/bin/sh\necho b >> "+marker+"\nexit 1\n")
	writeExec(t, filepath.Join(cfg, "entrypoint.d", "30-never.sh"),
		"#!/bin/sh\necho c >> "+marker+"\nexit 0\n")

	res := runZpinit(t, map[string]string{
		"ZPINIT_ENV_FILE": filepath.Join(t.TempDir(), "env"),
	}, "--config", cfg, "--", "/bin/echo", "should-not-run")
	if res.exitCode == 0 {
		t.Fatal("expected non-zero exit")
	}

	body, _ := os.ReadFile(marker)
	if string(body) != "a\nb\n" {
		t.Errorf("unexpected execution: %q", string(body))
	}
	if strings.Contains(res.stdout, "should-not-run") {
		t.Error("CMD should not have run after entrypoint.d failure")
	}
}

// #15: entrypoint_on_failure=continue lets later scripts and CMD run.
func TestEntrypointFailureContinue(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "log")

	writeFile(t, filepath.Join(cfg, "zpinit.toml"),
		`entrypoint_on_failure = "continue"`+"\n")
	writeExec(t, filepath.Join(cfg, "entrypoint.d", "10-fail.sh"),
		"#!/bin/sh\necho a >> "+marker+"\nexit 1\n")
	writeExec(t, filepath.Join(cfg, "entrypoint.d", "20-after.sh"),
		"#!/bin/sh\necho b >> "+marker+"\nexit 0\n")

	res := runZpinit(t, map[string]string{
		"ZPINIT_ENV_FILE": filepath.Join(t.TempDir(), "env"),
	}, "--config", cfg, "--", "/bin/echo", "ran")
	if res.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.exitCode, res.stderr)
	}

	body, _ := os.ReadFile(marker)
	if string(body) != "a\nb\n" {
		t.Errorf("expected both scripts to run, got %q", string(body))
	}
	if !strings.Contains(res.stdout, "ran") {
		t.Errorf("CMD should have run: %s", res.stdout)
	}
}

// #16: non-executable entrypoint.d files are silently skipped at runtime.
func TestEntrypointNonExecutableSkipped(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "log")

	writeFile(t, filepath.Join(cfg, "entrypoint.d", "10-noexec.sh"),
		"#!/bin/sh\necho noexec >> "+marker+"\n")
	writeExec(t, filepath.Join(cfg, "entrypoint.d", "20-yes.sh"),
		"#!/bin/sh\necho yes >> "+marker+"\n")

	res := runZpinit(t, map[string]string{
		"ZPINIT_ENV_FILE": filepath.Join(t.TempDir(), "env"),
	}, "--config", cfg, "--", "true")
	if res.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.exitCode, res.stderr)
	}

	body, _ := os.ReadFile(marker)
	if string(body) != "yes\n" {
		t.Errorf("expected only the executable script to run, got %q", string(body))
	}
}

// #17: ZPINIT_SKIP_ENTRYPOINT=1 bypasses the phase entirely.
func TestEntrypointSkippedViaEnv(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "log")

	writeExec(t, filepath.Join(cfg, "entrypoint.d", "10-touch.sh"),
		"#!/bin/sh\necho touched >> "+marker+"\n")

	res := runZpinit(t, map[string]string{
		"ZPINIT_SKIP_ENTRYPOINT": "1",
		"ZPINIT_ENV_FILE":        filepath.Join(t.TempDir(), "env"),
	}, "--config", cfg, "--", "true")
	if res.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.exitCode, res.stderr)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Error("entrypoint.d ran despite ZPINIT_SKIP_ENTRYPOINT=1")
	}
}

// #18: env propagation — script writes FOO=bar to env file, CMD sees FOO=bar.
func TestEnvPropagation(t *testing.T) {
	cfg := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "env")

	writeExec(t, filepath.Join(cfg, "entrypoint.d", "10-export.sh"),
		"#!/bin/sh\necho 'FOO=bar' >> "+envFile+"\n")

	// CMD prints whatever FOO is set to.
	res := runZpinit(t, map[string]string{
		"ZPINIT_ENV_FILE": envFile,
	}, "--config", cfg, "--", "/bin/sh", "-c", "echo FOO=$FOO")
	if res.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "FOO=bar") {
		t.Errorf("expected FOO=bar in CMD output, got %q", res.stdout)
	}
}

// #19 (host approximation): wrap mode exec'd CMD's stdout reaches us.
// True PID 1 verification belongs in smoke tests inside Docker.
func TestModeWrap(t *testing.T) {
	cfg := t.TempDir()
	res := runZpinit(t, map[string]string{
		"ZPINIT_ENV_FILE": filepath.Join(t.TempDir(), "env"),
	}, "--config", cfg, "--", "/bin/echo", "hello-wrap")
	if res.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "hello-wrap") {
		t.Errorf("stdout missing CMD output: %q", res.stdout)
	}
}

// #21: empty services + no CMD stays alive in supervise mode with zero
// runners. The control socket comes up; an operator can drop in service
// files and `zpctl reread` (or SIGHUP) to add them later. Exit only on
// SIGTERM.
func TestEmptyConfigStaysAlive(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
`, socket))

	envFile := filepath.Join(t.TempDir(), "env")
	cmd := exec.Command(zpinitBin, "--config", cfg)
	cmd.Env = append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// Wait for the control socket to appear: that's the proof zpinit
	// reached the supervise loop with zero services and didn't bail.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("control socket never appeared; stderr:\n%s", stderr.String())
	}

	// Process must still be alive; zero-service supervise mode does not
	// self-exit. Probe with signal 0.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("zpinit exited prematurely: %v\nstderr:\n%s", err, stderr.String())
	}

	// Clean shutdown on SIGTERM.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("zpinit exit error: %v\nstderr:\n%s", err, stderr.String())
	}
}

// #20: supervise mode actually runs services in filename order, stays
// alive as PID 1's stand-in, and shuts them down cleanly on SIGTERM.
func TestSuperviseMode(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "log")
	writeFile(t, filepath.Join(cfg, "services", "10_first.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "echo first >> %s; sleep 30"]
restart = "always"
stop_timeout = "3s"
`, marker))
	writeFile(t, filepath.Join(cfg, "services", "20_second.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "echo second >> %s; sleep 30"]
restart = "always"
stop_timeout = "3s"
`, marker))

	bin := zpinitBin
	envFile := filepath.Join(t.TempDir(), "env")
	cmd := exec.Command(bin, "--config", cfg)
	cmd.Env = append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Both services should write their markers shortly after boot.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil && strings.Contains(string(data), "first") && strings.Contains(string(data), "second") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	body, _ := os.ReadFile(marker)
	if !strings.Contains(string(body), "first") || !strings.Contains(string(body), "second") {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatalf("services did not run; marker = %q\nstderr:\n%s", body, stderr.String())
	}
	// Filename order should put "first" before "second".
	if i := strings.Index(string(body), "first"); i < 0 || i > strings.Index(string(body), "second") {
		t.Errorf("services ran out of order: %q", body)
	}

	// Shutdown should be quick.
	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("zpinit exit error: %v\nstderr:\n%s", err, stderr.String())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("shutdown took %v; want < 5s", elapsed)
	}
}

// #7 (host approximation): SIGHUP causes zpinit to re-read the config
// directory and start newly-added services without a restart.
func TestSuperviseSighupAddsService(t *testing.T) {
	cfg := t.TempDir()
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")

	writeFile(t, filepath.Join(cfg, "services", "10_a.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "touch %s; sleep 30"]
restart = "always"
stop_timeout = "1s"
`, a))

	envFile := filepath.Join(t.TempDir(), "env")
	cmd := exec.Command(zpinitBin, "--config", cfg)
	cmd.Env = append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// Wait for a's marker to confirm it's running.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(a); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatalf("a never started; stderr:\n%s", stderr.String())
	}

	// Drop a new service file and SIGHUP.
	writeFile(t, filepath.Join(cfg, "services", "20_b.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "touch %s; sleep 30"]
restart = "always"
stop_timeout = "1s"
`, b))
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}

	// b should appear shortly after the reload.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(b); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatalf("b never started after SIGHUP; stderr:\n%s", stderr.String())
	}

	// Clean shutdown.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("zpinit exit error: %v\nstderr:\n%s", err, stderr.String())
	}
}

// #6 (smoke version): a service that traps SIGTERM and runs sleep 30
// must still shut down promptly because the runner escalates to SIGKILL
// after stop_timeout. The whole shutdown should land within
// stop_timeout + a small grace, not the full 30s sleep.
func TestSuperviseStopEscalatesToKill(t *testing.T) {
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "services", "10_stubborn.toml"), `
command = ["/bin/sh", "-c", "trap '' TERM; sleep 30"]
restart = "always"
stop_timeout = "1s"
`)

	envFile := filepath.Join(t.TempDir(), "env")
	cmd := exec.Command(zpinitBin, "--config", cfg)
	cmd.Env = append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Let it boot.
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("zpinit exit error: %v\nstderr:\n%s", err, stderr.String())
	}
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Errorf("shutdown took %v; SIGKILL escalation should have killed the trap-TERM service", elapsed)
	}
	if !strings.Contains(stderr.String(), "SIGKILL") {
		t.Errorf("expected stderr to mention SIGKILL escalation; got:\n%s", stderr.String())
	}
}

// Phase 8: zpctl talks to zpinit over the control socket; status,
// stop, and shutdown all work end-to-end.
func TestZpctlEndToEnd(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf(`
control_socket = "%s"
`, socket))
	writeFile(t, filepath.Join(cfg, "services", "10_alpha.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"
`)
	writeFile(t, filepath.Join(cfg, "services", "20_beta.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"
`)

	envFile := filepath.Join(t.TempDir(), "env")
	zp := exec.Command(zpinitBin, "--config", cfg)
	zp.Env = append(os.Environ(), "ZPINIT_ENV_FILE="+envFile)
	var zpStderr bytes.Buffer
	zp.Stderr = &zpStderr
	if err := zp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = zp.Process.Signal(syscall.SIGKILL)
		_ = zp.Wait()
	})

	// Wait for the socket to appear (zpinit boots services then opens it).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("control socket never appeared; zpinit stderr:\n%s", zpStderr.String())
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

	// status: both services should be RUNNING.
	out, code := zpctl("status")
	if code != 0 {
		t.Fatalf("status exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "RUNNING") {
		t.Errorf("status output missing expected lines:\n%s", out)
	}
	if strings.Index(out, "alpha") > strings.Index(out, "beta") {
		t.Errorf("status not in filename order:\n%s", out)
	}

	// stop alpha
	out, code = zpctl("stop", "alpha")
	if code != 0 {
		t.Fatalf("stop alpha exit %d:\n%s", code, out)
	}

	// status should now show alpha STOPPED, beta RUNNING.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, _ = zpctl("status")
		if strings.Contains(out, "STOPPED") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(out, "STOPPED") {
		t.Errorf("alpha never reached STOPPED:\n%s", out)
	}

	// pid command without a name returns zpinit's pid.
	out, code = zpctl("pid")
	if code != 0 {
		t.Fatalf("pid exit %d:\n%s", code, out)
	}
	pid := strings.TrimSpace(out)
	if pid != fmt.Sprintf("%d", zp.Process.Pid) {
		t.Errorf("pid = %s, want %d", pid, zp.Process.Pid)
	}

	// shutdown ends the supervisor.
	_, _ = zpctl("shutdown")
	if err := zp.Wait(); err != nil {
		t.Fatalf("zpinit didn't exit cleanly: %v\nstderr:\n%s", err, zpStderr.String())
	}
}

// #22: CMD wins over services. Services TOML present + CMD provided →
// CMD execs, services never spawned.
func TestCmdWinsOverServices(t *testing.T) {
	cfg := t.TempDir()

	// A "service" that, if it ever ran, would write to a file.
	canary := filepath.Join(t.TempDir(), "canary")
	writeFile(t, filepath.Join(cfg, "services", "10_canary.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "echo ran > %s"]
`, canary))

	res := runZpinit(t, map[string]string{
		"ZPINIT_ENV_FILE": filepath.Join(t.TempDir(), "env"),
	}, "--config", cfg, "--", "/bin/echo", "wrapped")
	if res.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "wrapped") {
		t.Errorf("CMD output missing: %q", res.stdout)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("service ran in wrap mode — services should be ignored when CMD is provided")
	}
}
