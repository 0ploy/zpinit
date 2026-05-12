package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0ploy/zpinit/internal/config"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestParseNodeVersion(t *testing.T) {
	cases := []struct {
		in              string
		maj, min, patch int
		wantErr         bool
	}{
		{"v22.12.0\n", 22, 12, 0, false},
		{"22.13.5", 22, 13, 5, false},
		{"v22.13.0-rc.1\n", 22, 13, 0, false},
		{"v20.15.0", 20, 15, 0, false},
		{"v18.20.4", 18, 20, 4, false},
		{"node version 22.12.0", 0, 0, 0, true}, // prefix mismatch
		{"", 0, 0, 0, true},
		{"hello", 0, 0, 0, true},
	}
	for _, c := range cases {
		maj, min, patch, err := parseNodeVersion(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseNodeVersion(%q): want err, got %d.%d.%d", c.in, maj, min, patch)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseNodeVersion(%q): %v", c.in, err)
			continue
		}
		if maj != c.maj || min != c.min || patch != c.patch {
			t.Errorf("parseNodeVersion(%q) = %d.%d.%d, want %d.%d.%d", c.in, maj, min, patch, c.maj, c.min, c.patch)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		a, b, c, x, y, z int
		want             bool
	}{
		{22, 12, 0, 22, 12, 0, true},
		{22, 12, 5, 22, 12, 0, true},
		{22, 11, 99, 22, 12, 0, false},
		{23, 0, 0, 22, 12, 0, true},
		{18, 20, 4, 22, 12, 0, false},
	}
	for _, c := range cases {
		got := versionAtLeast(c.a, c.b, c.c, c.x, c.y, c.z)
		if got != c.want {
			t.Errorf("versionAtLeast(%d.%d.%d >= %d.%d.%d) = %v, want %v", c.a, c.b, c.c, c.x, c.y, c.z, got, c.want)
		}
	}
}

func TestReplicaLogPreview(t *testing.T) {
	if got := config.ReplicaLogPath("/logs/{index}/x.log", 1, 3); got != "/logs/1/x.log" {
		t.Errorf("got %q", got)
	}
	if got := config.ReplicaLogPath("inherit", 0, 3); got != "inherit" {
		t.Errorf("inherit must be unchanged, got %q", got)
	}
	// Without {index}, the preview helper is a no-op; the doctor's
	// upstream caller renders a "shared file" notice instead.
	if got := config.ReplicaLogPath("/var/log/x.log", 2, 4); got != "/var/log/x.log" {
		t.Errorf("got %q, want path unchanged when no placeholder", got)
	}
}

func TestRun_MissingConfigDirFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	checks := Run(missing)
	if !anyFail(checks) {
		t.Errorf("expected a FAIL for missing dir; got %v", checks)
	}
}

func TestRun_EmptyConfigDirOK(t *testing.T) {
	dir := t.TempDir()
	// Make zpinit's expected layout but with no services.
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Use a socket path inside tmp so state check sees no running instance.
	write(t, filepath.Join(dir, "zpinit.toml"),
		`control_socket = "`+filepath.Join(t.TempDir(), "nope.sock")+`"`+"\n", 0o644)
	checks := Run(dir)
	if anyFail(checks) {
		t.Errorf("expected no fails for empty config; got %v", checks)
	}
}

func TestRun_MissingCommandFails(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_foo.toml"),
		`command = ["/opt/definitely-not-a-real-binary"]`, 0o644)
	write(t, filepath.Join(dir, "zpinit.toml"),
		`control_socket = "`+filepath.Join(t.TempDir(), "nope.sock")+`"`+"\n", 0o644)
	checks := Run(dir)
	if !anyFail(checks) {
		t.Errorf("expected FAIL for missing command; checks: %v", checks)
	}
	var found bool
	for _, c := range checks {
		if c.Status == StatusFail && strings.Contains(c.Detail, "/opt/definitely-not-a-real-binary") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a FAIL mentioning the missing binary path; got %v", checks)
	}
}

func TestRun_ReplicasLogPreview_Shared(t *testing.T) {
	// Default (no {index}) emits a "shared file" notice with a hint
	// about the placeholder.
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "consumer.log")
	write(t, filepath.Join(dir, "services", "10_consumer.toml"),
		`command = ["/bin/sleep"]
replicas = 3
[log]
stdout = "`+logPath+`"
`, 0o644)
	write(t, filepath.Join(dir, "zpinit.toml"),
		`control_socket = "`+filepath.Join(t.TempDir(), "nope.sock")+`"`+"\n", 0o644)
	preview := findLogPathsCheck(t, Run(dir), "consumer")
	if !strings.Contains(preview.Detail, "share") || !strings.Contains(preview.Detail, "{index}") {
		t.Errorf("expected a shared-file notice mentioning {index}; got %q", preview.Detail)
	}
}

func TestRun_ReplicasLogPreview_PlaceholderFanOut(t *testing.T) {
	// With {index} the doctor expands every replica's path so the
	// operator can confirm the layout before boot.
	dir := t.TempDir()
	logBase := filepath.Join(t.TempDir(), "consumer")
	write(t, filepath.Join(dir, "services", "10_consumer.toml"),
		`command = ["/bin/sleep"]
replicas = 3
[log]
stdout = "`+logBase+`-{index}.log"
`, 0o644)
	write(t, filepath.Join(dir, "zpinit.toml"),
		`control_socket = "`+filepath.Join(t.TempDir(), "nope.sock")+`"`+"\n", 0o644)
	preview := findLogPathsCheck(t, Run(dir), "consumer")
	if !strings.Contains(preview.Detail, "consumer-0.log") || !strings.Contains(preview.Detail, "consumer-2.log") {
		t.Errorf("preview missing expected expansions: %q", preview.Detail)
	}
}

// TestRun_NodeRuntimeAbsolutePath: when a service uses an absolute
// node binary path, doctor must probe THAT binary, not whatever
// `node` resolves to on PATH. Otherwise a config that runs an old
// `/opt/node-v20/bin/node` could be certified by the doctor as
// "supports reusePort" because a newer node on PATH does — exactly
// the EADDRINUSE failure the doctor is supposed to catch.
func TestRun_NodeRuntimeAbsolutePath(t *testing.T) {
	binDir := t.TempDir()
	// Synthetic node binary that reports v20.5.0 (below 22.12.0 floor).
	// Named "node" so doctor recognizes it; lives in its own tempdir
	// so it never collides with whatever node is on the test host's PATH.
	fakePath := filepath.Join(binDir, "node")
	write(t, fakePath,
		"#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'v20.5.0'; exit 0; fi\nsleep 30\n",
		0o755)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_api.toml"),
		`command = ["`+fakePath+`", "/app/server.js"]
replicas = 4
`, 0o644)
	write(t, filepath.Join(dir, "zpinit.toml"),
		`control_socket = "`+filepath.Join(t.TempDir(), "nope.sock")+`"`+"\n", 0o644)

	checks := Run(dir)
	// Find any runtime check that mentions our fake binary's path.
	var nodeCheck *Check
	for i, c := range checks {
		if c.Category == "runtimes" && strings.Contains(c.Detail, "20.5.0") {
			nodeCheck = &checks[i]
			break
		}
	}
	if nodeCheck == nil {
		t.Fatalf("expected a node runtime check probing the absolute path; got %v", checks)
	}
	if nodeCheck.Status != StatusWarn {
		t.Errorf("expected WARN for node 20.5.0 + replicas > 1; got %s (%s)", nodeCheck.Status, nodeCheck.Detail)
	}
	if !strings.Contains(nodeCheck.Detail, "EADDRINUSE") {
		t.Errorf("expected mention of EADDRINUSE in WARN; got %q", nodeCheck.Detail)
	}
	// The check label should reference the configured (absolute) path,
	// not just "node" — so the operator can tell which binary was probed.
	if !strings.Contains(nodeCheck.Name, fakePath) {
		t.Errorf("expected check name to reference configured path %q; got %q", fakePath, nodeCheck.Name)
	}
}

func findLogPathsCheck(t *testing.T, checks []Check, svc string) *Check {
	t.Helper()
	for i, c := range checks {
		if strings.Contains(c.Name, svc) && strings.Contains(c.Name, "log paths") {
			return &checks[i]
		}
	}
	t.Fatalf("no %s log-paths check found in %v", svc, checks)
	return nil
}

func anyFail(checks []Check) bool {
	for _, c := range checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}
