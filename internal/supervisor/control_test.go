package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLastBytes_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.log")
	if err := os.WriteFile(real, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if _, err := readLastBytes(link, 4096); err == nil {
		t.Fatal("readLastBytes(symlink): want error, got nil")
	}

	if got, err := readLastBytes(real, 4096); err != nil {
		t.Fatalf("readLastBytes(real): %v", err)
	} else if got != "hello\n" {
		t.Errorf("readLastBytes(real) = %q, want %q", got, "hello\n")
	}
}

func TestReadLastBytes_RejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	if _, err := readLastBytes(dir, 4096); err == nil {
		t.Fatal("readLastBytes(directory): want error, got nil")
	} else if !strings.Contains(err.Error(), "regular") && !strings.Contains(err.Error(), "directory") {
		// Either our explicit check fired, or the OS rejected it
		// (some platforms reject directory reads earlier). Either is
		// acceptable; we just want it not to silently succeed.
		t.Logf("readLastBytes(directory) error: %v", err)
	}
}
