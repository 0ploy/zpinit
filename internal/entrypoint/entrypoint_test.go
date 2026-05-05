package entrypoint

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeNonExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestListScripts_Filtering(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "10-a.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "20-b.sh"), "#!/bin/sh\nexit 0\n")
	writeNonExec(t, filepath.Join(dir, "30-noexec.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, ".hidden"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "40-disabled.disabled"), "#!/bin/sh\nexit 0\n")

	got, err := listScripts(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "10-a.sh"),
		filepath.Join(dir, "20-b.sh"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMergeEnvFile_Cases(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "env")
	content := strings.Join([]string{
		"FOO=bar",
		"",
		"# comment",
		"BAZ=value with spaces",
		"NOEQUALS",
		"=novalue",
		"INVALID-KEY=ignored",
		"UNICODE=héllo",
		"OVERRIDE=first",
		"OVERRIDE=second",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	env := map[string]string{}
	if err := mergeEnvFile(env, path, log); err != nil {
		t.Fatal(err)
	}
	if env["FOO"] != "bar" {
		t.Errorf("FOO = %q", env["FOO"])
	}
	if env["BAZ"] != "value with spaces" {
		t.Errorf("BAZ = %q", env["BAZ"])
	}
	if env["UNICODE"] != "héllo" {
		t.Errorf("UNICODE = %q", env["UNICODE"])
	}
	if env["OVERRIDE"] != "second" {
		t.Errorf("OVERRIDE = %q (want last-wins)", env["OVERRIDE"])
	}
	if _, ok := env["INVALID-KEY"]; ok {
		t.Error("INVALID-KEY should not be set (hyphen)")
	}
	if _, ok := env["NOEQUALS"]; ok {
		t.Error("NOEQUALS should not be set (no =)")
	}
	logged := buf.String()
	if !strings.Contains(logged, "malformed env line") || !strings.Contains(logged, "invalid env key") {
		t.Errorf("expected warnings about malformed and invalid lines: %s", logged)
	}
}

func TestMergeEnvFile_Missing(t *testing.T) {
	env := map[string]string{}
	if err := mergeEnvFile(env, "/nonexistent/path", discardLogger()); err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
}

func TestRun_NoDir(t *testing.T) {
	tmp := t.TempDir()
	out, err := Run(context.Background(), Config{
		Dir:     filepath.Join(tmp, "does-not-exist"),
		EnvFile: filepath.Join(tmp, "env"),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("missing dir should be a no-op: %v", err)
	}
	// env still includes container env (PATH etc.).
	if len(out) == 0 {
		t.Error("expected at least the container env")
	}
}

func TestRun_TimeoutKills(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "ep")
	writeExec(t, filepath.Join(dir, "10-stuck.sh"), "#!/bin/sh\ntrap '' TERM\nsleep 30\n")

	start := time.Now()
	_, err := Run(context.Background(), Config{
		Dir:           dir,
		ScriptTimeout: 200 * time.Millisecond,
		KillGrace:     200 * time.Millisecond,
		Logger:        discardLogger(),
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}
