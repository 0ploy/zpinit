//go:build integration

package integration

import (
	"path/filepath"
	"strings"
	"testing"
)

// The one-shot CLI modes (--check-config, --plan, --version) are what
// image builds and CI pipelines call; pin their exit codes and the
// lines a pipeline would grep for.

func TestCheckConfig(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := t.TempDir()
		writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sleep", "30"]
`)
		res := runZpinit(t, nil, "--check-config", cfg)
		if res.exitCode != 0 || !strings.Contains(res.stdout, "config OK: 1 service(s)") {
			t.Errorf("exit %d stdout %q stderr %q", res.exitCode, res.stdout, res.stderr)
		}
	})
	t.Run("skipped file fails", func(t *testing.T) {
		cfg := t.TempDir()
		writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sleep", "30"]
`)
		writeFile(t, filepath.Join(cfg, "services", "20_bad.toml"), `
command = "not-an-array"
`)
		res := runZpinit(t, nil, "--check-config", cfg)
		if res.exitCode != 1 || !strings.Contains(res.stderr, "skipped 20_bad.toml") || !strings.Contains(res.stderr, "config FAILED") {
			t.Errorf("exit %d stdout %q stderr %q", res.exitCode, res.stdout, res.stderr)
		}
	})
	t.Run("bad globals fail", func(t *testing.T) {
		cfg := t.TempDir()
		writeFile(t, filepath.Join(cfg, "zpinit.toml"), `control_socket = "relative.sock"`+"\n")
		res := runZpinit(t, nil, "--check-config", cfg)
		if res.exitCode != 1 || res.stderr == "" {
			t.Errorf("exit %d stdout %q stderr %q", res.exitCode, res.stdout, res.stderr)
		}
	})
	t.Run("missing dir fails", func(t *testing.T) {
		res := runZpinit(t, nil, "--check-config", filepath.Join(t.TempDir(), "nope"))
		if res.exitCode != 1 {
			t.Errorf("exit %d stdout %q stderr %q", res.exitCode, res.stdout, res.stderr)
		}
	})
}

func TestPlan(t *testing.T) {
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), `exit_code_from = "worker"`+"\n")
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `
command = ["/bin/sleep", "30"]
`)
	writeFile(t, filepath.Join(cfg, "services", "20_worker.toml"), `
command = ["/bin/sleep", "30"]
restart = "never"
`)
	writeExec(t, filepath.Join(cfg, "entrypoint.d", "10-touch.sh"), "#!/bin/sh\ntouch /nonexistent/should-not-run\n")

	t.Run("supervise", func(t *testing.T) {
		res := runZpinit(t, nil, "--plan", "--config", cfg)
		if res.exitCode != 0 {
			t.Fatalf("exit %d stderr %q", res.exitCode, res.stderr)
		}
		for _, want := range []string{"mode: supervise", "services (2, in filename order)", "app", "worker", "10-touch.sh", "exit_code_from: worker"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("plan missing %q:\n%s", want, res.stdout)
			}
		}
	})
	t.Run("wrap", func(t *testing.T) {
		res := runZpinit(t, nil, "--plan", "--config", cfg, "php", "cli")
		if res.exitCode != 0 {
			t.Fatalf("exit %d stderr %q", res.exitCode, res.stderr)
		}
		for _, want := range []string{"mode: wrap", `argv  ["php" "cli"]`, "services/ is ignored"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("plan missing %q:\n%s", want, res.stdout)
			}
		}
	})
}

func TestVersion(t *testing.T) {
	res := runZpinit(t, nil, "--version")
	if res.exitCode != 0 || strings.TrimSpace(res.stdout) == "" {
		t.Errorf("exit %d stdout %q stderr %q", res.exitCode, res.stdout, res.stderr)
	}
}
