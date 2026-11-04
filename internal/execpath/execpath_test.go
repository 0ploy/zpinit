package execpath

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFind(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "noexec"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := Find("tool", "/nonexistent:"+dir); err != nil || got != bin {
		t.Errorf("Find(tool) = %q, %v; want %q", got, err, bin)
	}
	for _, name := range []string{"noexec", "missing"} {
		if _, err := Find(name, dir); !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("Find(%s) err = %v, want ErrNotFound", name, err)
		}
	}
	if got, err := Find(bin, ""); err != nil || got != bin {
		t.Errorf("Find(abs) = %q, %v", got, err)
	}
	if _, err := Find(filepath.Join(dir, "missing"), ""); err == nil {
		t.Error("Find(abs missing) succeeded")
	}
	// A match through a relative entry is refused, as exec.LookPath does.
	t.Chdir(dir)
	if _, err := Find("tool", "."); !errors.Is(err, exec.ErrDot) {
		t.Errorf("Find via '.' err = %v, want ErrDot", err)
	}
}

// Find never consults the process's own PATH.
func TestFindIgnoresProcessPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := Find("sh", "/nonexistent"); err == nil {
		t.Error("Find used the process PATH")
	}
}

func TestFromEnv(t *testing.T) {
	if p, ok := FromEnv([]string{"A=1", "PATH=/a", "PATH=/b"}); !ok || p != "/b" {
		t.Errorf("FromEnv = %q, %v", p, ok)
	}
	if _, ok := FromEnv([]string{"PATHX=/a"}); ok {
		t.Error("FromEnv matched PATHX")
	}
}
