package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanServiceFiles(t *testing.T) {
	root := t.TempDir()
	sdir := filepath.Join(root, "services")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(sdir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Prefix-stripped name.
	write("10_redis.toml", `command = ["redis-server"]`)
	// name= override wins over the filename.
	write("20-frontend.toml", `command = ["nginx"]`+"\n"+`name = "web"`)
	// Disabled file still resolves, marked not enabled.
	write("30_worker.toml.disabled", `command = ["worker"]`)
	// Skipped: dotfile (editor swap) and non-toml.
	write(".40_swap.toml.swp", `garbage`)
	write("notes.txt", `ignore me`)

	files, err := ScanServiceFiles(root)
	if err != nil {
		t.Fatalf("ScanServiceFiles: %v", err)
	}

	byName := map[string]ServiceFileInfo{}
	for _, f := range files {
		byName[f.Name] = f
	}
	if len(byName) != 3 {
		t.Fatalf("got %d resolved services %v, want 3", len(byName), byName)
	}

	redis, ok := byName["redis"]
	if !ok || !redis.Enabled {
		t.Errorf("redis: %+v (want enabled)", redis)
	}
	if filepath.Base(redis.Path) != "10_redis.toml" || !filepath.IsAbs(redis.Path) {
		t.Errorf("redis path = %q, want absolute .../10_redis.toml", redis.Path)
	}

	web, ok := byName["web"]
	if !ok || !web.Enabled {
		t.Errorf("web (name override): %+v", web)
	}

	worker, ok := byName["worker"]
	if !ok {
		t.Fatalf("disabled worker should still resolve")
	}
	if worker.Enabled {
		t.Errorf("worker should be marked disabled")
	}
	if filepath.Base(worker.Path) != "30_worker.toml.disabled" {
		t.Errorf("worker path = %q, want .../30_worker.toml.disabled", worker.Path)
	}
}

func TestScanServiceFiles_MissingDir(t *testing.T) {
	// No services/ subdir: not an error, empty result.
	files, err := ScanServiceFiles(t.TempDir())
	if err != nil {
		t.Fatalf("ScanServiceFiles(missing): %v", err)
	}
	if len(files) != 0 {
		t.Errorf("want empty, got %v", files)
	}
}
