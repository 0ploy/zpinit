//go:build integration

package integration

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctor_CleanConfigExitsZero: a well-formed config dir with no
// problems produces exit code 0 and a summary line.
func TestDoctor_CleanConfigExitsZero(t *testing.T) {
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "zpinit.toml"),
		`control_socket = "`+filepath.Join(t.TempDir(), "nope.sock")+`"`+"\n")
	writeFile(t, filepath.Join(cfg, "services", "10_demo.toml"), `
command = ["/bin/sleep", "30"]
`)
	out, code := runDoctor(t, nil, cfg)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\nout:\n%s", code, out)
	}
	if !strings.Contains(out, "summary:") {
		t.Errorf("missing summary line:\n%s", out)
	}
	if !strings.Contains(out, "0 fail") {
		t.Errorf("expected 0 fail in summary:\n%s", out)
	}
}

// TestDoctor_MissingCommandFails: a service with an absent command
// produces exit 1 and a FAIL row that names the missing binary.
func TestDoctor_MissingCommandFails(t *testing.T) {
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "zpinit.toml"),
		`control_socket = "`+filepath.Join(t.TempDir(), "nope.sock")+`"`+"\n")
	writeFile(t, filepath.Join(cfg, "services", "10_broken.toml"), `
command = ["/opt/definitely-not-a-real-binary-for-doctor-test"]
`)
	out, code := runDoctor(t, nil, cfg)
	if code != 1 {
		t.Errorf("exit=%d, want 1 (FAIL)\nout:\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("expected FAIL row:\n%s", out)
	}
	if !strings.Contains(out, "/opt/definitely-not-a-real-binary-for-doctor-test") {
		t.Errorf("expected the missing binary name to appear:\n%s", out)
	}
}

// TestDoctor_OldNodeWarnsForReplicas: when a node service has
// replicas > 1 and the node binary on PATH reports a version below
// the reusePort floor (22.12.0), doctor must produce a WARN and exit
// with code 2 (warnings-only). The fake node binary is built into a
// tempdir and PATH-prepended for this subtest.
func TestDoctor_OldNodeWarnsForReplicas(t *testing.T) {
	fakeBinDir := t.TempDir()
	// Fake node: prints v20.0.0 (below the 22.12.0 floor).
	writeExec(t, filepath.Join(fakeBinDir, "node"),
		"#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'v20.0.0'; exit 0; fi\nsleep 30\n")

	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "zpinit.toml"),
		`control_socket = "`+filepath.Join(t.TempDir(), "nope.sock")+`"`+"\n")
	writeFile(t, filepath.Join(cfg, "services", "10_api.toml"), `
command = ["node", "app.js"]
replicas = 4
`)
	env := map[string]string{
		"PATH": fakeBinDir + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	out, code := runDoctor(t, env, cfg)
	if code != 2 {
		t.Errorf("exit=%d, want 2 (WARN only)\nout:\n%s", code, out)
	}
	if !strings.Contains(out, "WARN") {
		t.Errorf("expected WARN row:\n%s", out)
	}
	if !strings.Contains(out, "EADDRINUSE") {
		t.Errorf("expected mention of EADDRINUSE in WARN:\n%s", out)
	}
}

// runDoctor invokes the built zpinit binary with --doctor and the
// given config dir. Extra env vars (e.g. a synthetic PATH) replace
// the default environment.
func runDoctor(t *testing.T, env map[string]string, configDir string) (string, int) {
	t.Helper()
	cmd := exec.Command(zpinitBin, "--doctor", "--config", configDir)
	if env != nil {
		envv := make([]string, 0, len(env))
		for k, v := range env {
			envv = append(envv, k+"="+v)
		}
		cmd.Env = envv
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run --doctor: %v\nstderr:\n%s", err, errBuf.String())
	}
	return out.String() + errBuf.String(), code
}
