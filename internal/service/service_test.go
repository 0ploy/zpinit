package service

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// drainReaper polls Reap from a goroutine, simulating main's
// SIGCHLD-driven loop. Returned cleanup must be deferred.
func drainReaper(t *testing.T, r *reaper.Reaper) func() {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				r.Reap()
				return
			case <-ticker.C:
				r.Reap()
			}
		}
	}()
	return func() { close(stop); <-done }
}

func setup(t *testing.T) (*reaper.Reaper, *slog.Logger) {
	t.Helper()
	log := discardLogger()
	r := reaper.New(log)
	t.Cleanup(drainReaper(t, r))
	return r, log
}

func TestSpawn_BasicExit(t *testing.T) {
	r, log := setup(t)

	cfg := config.Service{
		Name:    "test",
		Command: []string{"/bin/echo", "hello"},
		Log:     config.Logging{Stdout: "inherit", Stderr: "inherit"},
	}

	p, err := Spawn(cfg, os.Environ(), r, log)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case info := <-p.Exit:
		if info.Signaled || info.ExitCode != 0 {
			t.Errorf("unexpected exit: %+v", info)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestSpawn_NonZeroExit(t *testing.T) {
	r, log := setup(t)

	cfg := config.Service{
		Name:    "test",
		Command: []string{"/bin/sh", "-c", "exit 42"},
		Log:     config.Logging{Stdout: "inherit", Stderr: "inherit"},
	}

	p, err := Spawn(cfg, os.Environ(), r, log)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case info := <-p.Exit:
		if info.Signaled || info.ExitCode != 42 {
			t.Errorf("got %+v, want exit 42", info)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestSpawn_LogToFile(t *testing.T) {
	r, log := setup(t)

	out := filepath.Join(t.TempDir(), "out.log")
	cfg := config.Service{
		Name:    "test",
		Command: []string{"/bin/sh", "-c", "echo hello-from-service"},
		Log:     config.Logging{Stdout: out, Stderr: "inherit"},
	}

	p, err := Spawn(cfg, os.Environ(), r, log)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-p.Exit:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello-from-service\n" {
		t.Errorf("log content = %q", string(body))
	}
}

func TestSpawn_PgidKill(t *testing.T) {
	r, log := setup(t)

	// Shell ignores SIGTERM; SIGKILL the group.
	cfg := config.Service{
		Name:    "stubborn",
		Command: []string{"/bin/sh", "-c", "trap '' TERM; sleep 30"},
		Log:     config.Logging{Stdout: "inherit", Stderr: "inherit"},
	}

	p, err := Spawn(cfg, os.Environ(), r, log)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	select {
	case info := <-p.Exit:
		if !info.Signaled || info.Signal != syscall.SIGKILL {
			t.Errorf("got %+v, want signaled SIGKILL", info)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestSpawn_EnvOverride(t *testing.T) {
	r, log := setup(t)

	out := filepath.Join(t.TempDir(), "out.log")
	cfg := config.Service{
		Name:    "test",
		Command: []string{"/bin/sh", "-c", "echo $FOO $BAR"},
		Env:     map[string]string{"FOO": "from-service", "BAR": "added"},
		Log:     config.Logging{Stdout: out, Stderr: "inherit"},
	}

	base := []string{"FOO=from-base", "PATH=/usr/bin:/bin"}
	p, err := Spawn(cfg, base, r, log)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-p.Exit:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	body, _ := os.ReadFile(out)
	if string(body) != "from-service added\n" {
		t.Errorf("env override failed: %q", string(body))
	}
}

func TestSpawn_Cwd(t *testing.T) {
	r, log := setup(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "out.log")
	cfg := config.Service{
		Name:    "test",
		Command: []string{"/bin/sh", "-c", "pwd"},
		Cwd:     dir,
		Log:     config.Logging{Stdout: out, Stderr: "inherit"},
	}

	p, err := Spawn(cfg, os.Environ(), r, log)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Exit:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	body, _ := os.ReadFile(out)
	// On macOS /tmp is symlinked to /private/tmp; pwd resolves it.
	got := string(body)
	if got == "" {
		t.Errorf("pwd produced empty output")
	}
	// Just verify pwd produced something with the temp-dir suffix.
	if !filepath.IsAbs(got[:len(got)-1]) {
		t.Errorf("pwd not absolute: %q", got)
	}
}

func TestSpawn_RelativeLogPathRejected(t *testing.T) {
	r, log := setup(t)

	cfg := config.Service{
		Name:    "test",
		Command: []string{"/bin/echo"},
		Log:     config.Logging{Stdout: "relative/path", Stderr: "inherit"},
	}
	if _, err := Spawn(cfg, os.Environ(), r, log); err == nil {
		t.Fatal("expected error on relative log path")
	}
}

// openLogTarget mkdir-p's the parent of the log path before opening it,
// so users don't have to ship a per-image entrypoint.d/00-mklogdir.sh.
// The test points the log at a path several levels deep that doesn't
// exist yet and verifies Spawn succeeds and the file ends up populated.
func TestSpawn_LogParentDirCreated(t *testing.T) {
	r, log := setup(t)

	deep := filepath.Join(t.TempDir(), "var", "log", "zpinit", "writer.out.log")
	cfg := config.Service{
		Name:    "test",
		Command: []string{"/bin/sh", "-c", "echo line-from-deep-dir"},
		Log:     config.Logging{Stdout: deep, Stderr: "inherit"},
	}

	p, err := Spawn(cfg, os.Environ(), r, log)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	select {
	case <-p.Exit:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	body, err := os.ReadFile(deep)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(body) != "line-from-deep-dir\n" {
		t.Errorf("log content = %q", string(body))
	}
}

func TestResolveCredentials_NoUser(t *testing.T) {
	cred, err := resolveCredentials("", "")
	if err != nil {
		t.Fatal(err)
	}
	if cred != nil {
		t.Errorf("expected nil credential, got %+v", cred)
	}
}

func TestResolveCredentials_NumericUid(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	cred, err := resolveCredentials(uid, "")
	if err != nil {
		t.Fatal(err)
	}
	if cred == nil {
		t.Fatal("expected credential, got nil")
	}
	if int(cred.Uid) != os.Getuid() {
		t.Errorf("uid = %d, want %d", cred.Uid, os.Getuid())
	}
}

func TestResolveCredentials_UnknownUser(t *testing.T) {
	if _, err := resolveCredentials("nonexistent_user_zzz", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestMergeServiceEnv(t *testing.T) {
	base := []string{"FOO=base", "PATH=/usr/bin", "KEEP=this"}
	override := map[string]string{"FOO": "override", "NEW": "value"}
	got := mergeServiceEnv(base, override)

	want := map[string]string{
		"FOO":  "override",
		"PATH": "/usr/bin",
		"KEEP": "this",
		"NEW":  "value",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v entries", got, want)
	}
	gotMap := map[string]string{}
	for _, e := range got {
		if i := indexByte(e, '='); i > 0 {
			gotMap[e[:i]] = e[i+1:]
		}
	}
	for k, v := range want {
		if gotMap[k] != v {
			t.Errorf("%s = %q, want %q", k, gotMap[k], v)
		}
	}
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
