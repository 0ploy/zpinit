package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNameFromFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"10_mysql.toml", "mysql"},
		{"30-nginx.toml", "nginx"},
		{"cron.toml", "cron"},
		{"99_worker.toml", "worker"},
		{"5redis.toml", "redis"},
		{"foo-bar.toml", "foo-bar"},
	}
	for _, c := range cases {
		if got := nameFromFilename(c.in); got != c.want {
			t.Errorf("nameFromFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoad_ValidMinimal(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_redis.toml"), `command = ["redis-server"]`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Services) != 1 {
		t.Fatalf("want 1 service, got %d", len(cfg.Services))
	}
	s := cfg.Services[0]
	if s.Name != "redis" {
		t.Errorf("name = %q, want redis", s.Name)
	}
	if s.Restart != RestartAlways {
		t.Errorf("restart = %q, want always", s.Restart)
	}
	if s.BackoffInitial.Std() != time.Second {
		t.Errorf("backoff_initial = %v, want 1s", s.BackoffInitial.Std())
	}
	if s.BackoffMax.Std() != 30*time.Second {
		t.Errorf("backoff_max = %v, want 30s", s.BackoffMax.Std())
	}
	if s.StopSignal != "TERM" {
		t.Errorf("stop_signal = %q, want TERM (inherited)", s.StopSignal)
	}
	if !s.IsReloadable() {
		t.Error("default reloadable should be true")
	}
}

func TestLoad_GlobalDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Globals.BootTimeout.Std() != 60*time.Second {
		t.Errorf("boot_timeout = %v", cfg.Globals.BootTimeout.Std())
	}
	if cfg.Globals.EntrypointOnFailure != EntrypointFail {
		t.Errorf("entrypoint_on_failure = %q", cfg.Globals.EntrypointOnFailure)
	}
	if cfg.Globals.ExitCodeFrom != "default" {
		t.Errorf("exit_code_from = %q", cfg.Globals.ExitCodeFrom)
	}
	if cfg.Globals.ControlSocket != "/run/zpinit.sock" {
		t.Errorf("control_socket = %q", cfg.Globals.ControlSocket)
	}
}

func TestLoad_GlobalsFromFile(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `
boot_timeout = "2m"
default_stop_signal = "INT"
control_socket = "/tmp/zpinit.sock"
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Globals.BootTimeout.Std() != 2*time.Minute {
		t.Errorf("boot_timeout = %v", cfg.Globals.BootTimeout.Std())
	}
	if cfg.Globals.DefaultStopSignal != "INT" {
		t.Errorf("default_stop_signal = %q", cfg.Globals.DefaultStopSignal)
	}
	if cfg.Globals.ControlSocket != "/tmp/zpinit.sock" {
		t.Errorf("control_socket = %q", cfg.Globals.ControlSocket)
	}
}

func TestLoad_RejectsRelativeControlSocket(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `control_socket = "zpinit.sock"`+"\n")
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load: want error for relative control_socket, got nil")
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("Load: error %q does not mention absolute path", err)
	}
}

func TestLoad_NameOverride(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_bar.toml"), `
name = "foo"
command = ["sleep", "1"]
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Services[0].Name != "foo" {
		t.Errorf("name = %q, want foo", cfg.Services[0].Name)
	}
	if cfg.Services[0].Filename != "10_bar.toml" {
		t.Errorf("filename = %q, want 10_bar.toml", cfg.Services[0].Filename)
	}
}

func TestLoad_NameCollision(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_redis.toml"), `command = ["redis-server"]`)
	write(t, filepath.Join(dir, "services", "20_redis.toml"), `command = ["redis-server"]`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(err.Error(), "name collision") {
		t.Errorf("error %q does not mention collision", err)
	}
	if !strings.Contains(err.Error(), "10_redis.toml") || !strings.Contains(err.Error(), "20_redis.toml") {
		t.Errorf("error should name both files: %v", err)
	}
}

func TestLoad_InvalidRestart(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "redis.toml"), `
command = ["redis-server"]
restart = "sometimes"
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("error should mention restart: %v", err)
	}
}

func TestLoad_MissingCommand(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "redis.toml"), `name = "redis"`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "command") {
		t.Errorf("error should mention command: %v", err)
	}
}

func TestLoad_ExitCodeFromUnknown(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `exit_code_from = "ghost"`)
	write(t, filepath.Join(dir, "services", "redis.toml"), `command = ["redis"]`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exit_code_from") {
		t.Errorf("error should mention exit_code_from: %v", err)
	}
}

func TestLoad_ExitCodeFromKnown(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `exit_code_from = "redis"`)
	write(t, filepath.Join(dir, "services", "redis.toml"), `command = ["redis-server"]`)
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_OrderingIsByFilename(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "30_c.toml"), `command = ["sleep", "1"]`)
	write(t, filepath.Join(dir, "services", "10_a.toml"), `command = ["sleep", "1"]`)
	write(t, filepath.Join(dir, "services", "20_b.toml"), `command = ["sleep", "1"]`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10_a.toml", "20_b.toml", "30_c.toml"}
	for i, w := range want {
		if cfg.Services[i].Filename != w {
			t.Errorf("[%d] = %q, want %q", i, cfg.Services[i].Filename, w)
		}
	}
}

func TestLoad_NonTomlFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "redis.toml"), `command = ["redis-server"]`)
	write(t, filepath.Join(dir, "services", "README"), "not a config")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 1 {
		t.Errorf("want 1 service, got %d", len(cfg.Services))
	}
}

func TestLoad_ReloadableFlag(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_a.toml"), `command = ["x"]`)
	write(t, filepath.Join(dir, "services", "20_b.toml"), `
command = ["x"]
reloadable = false
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Services[0].IsReloadable() {
		t.Error("a should default reloadable=true")
	}
	if cfg.Services[1].IsReloadable() {
		t.Error("b should be reloadable=false")
	}
}

func TestLoad_UnknownKey(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "redis.toml"), `
command = ["redis-server"]
restartt = "always"
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error on typo'd key")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention unknown key: %v", err)
	}
}

func TestLoad_ReadyDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "redis.toml"), `
command = ["redis-server"]

[ready]
command = ["redis-cli", "ping"]
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Services[0].Ready
	if r == nil {
		t.Fatal("expected [ready] to be parsed")
	}
	if r.Interval.Std() != 500*time.Millisecond {
		t.Errorf("interval = %v, want 500ms", r.Interval.Std())
	}
	if r.Timeout.Std() != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", r.Timeout.Std())
	}
	if r.OnTimeout != ReadyFail {
		t.Errorf("on_timeout = %q, want fail", r.OnTimeout)
	}
}

func TestLoad_ReadyMissingCommand(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "redis.toml"), `
command = ["redis-server"]

[ready]
interval = "1s"
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "[ready].command") {
		t.Errorf("error should mention [ready].command: %v", err)
	}
}

func TestLoad_EntrypointNonExecutableWarns(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "redis.toml"), `command = ["redis"]`)
	write(t, filepath.Join(dir, "entrypoint.d", "10-noexec.sh"), "#!/bin/sh\necho hi\n")
	// File written via os.WriteFile defaults to 0o644 — not executable.

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "10-noexec.sh") {
		t.Errorf("expected warning about non-executable script, got %v", cfg.Warnings)
	}
}

func TestLoad_NameWithInvalidChars(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_redis.toml"), `
name = "redis/server"
command = ["redis-server"]
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error on invalid name")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention name: %v", err)
	}
}

func TestLoad_DurationParseError(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "redis.toml"), `
command = ["redis-server"]
backoff_initial = "two seconds"
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected duration parse error")
	}
}

func TestLoad_GlobalsEnvParses(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `
[env]
APP_ENV = "production"
LOG_LEVEL = "info"
EMPTY = ""
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Globals.Env["APP_ENV"]; got != "production" {
		t.Errorf("APP_ENV = %q, want production", got)
	}
	if got := cfg.Globals.Env["LOG_LEVEL"]; got != "info" {
		t.Errorf("LOG_LEVEL = %q, want info", got)
	}
	if v, ok := cfg.Globals.Env["EMPTY"]; !ok || v != "" {
		t.Errorf("EMPTY = (%q, %v), want (\"\", true)", v, ok)
	}
}

func TestLoad_GlobalsEnvInvalidKey(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `
[env]
"BAD-KEY" = "x"
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected validation error for hyphen in env key")
	}
	if !strings.Contains(err.Error(), "env key") {
		t.Errorf("error should mention env key: %v", err)
	}
}

func TestLoad_ReplicasDefault(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_a.toml"), `command = ["x"]`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Services[0].Replicas; got != 1 {
		t.Errorf("default Replicas = %d, want 1", got)
	}
}

func TestLoad_ReplicasExplicit(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_a.toml"), `
command = ["x"]
replicas = 4
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Services[0].Replicas; got != 4 {
		t.Errorf("Replicas = %d, want 4", got)
	}
}

func TestLoad_ReplicasNegative(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_a.toml"), `
command = ["x"]
replicas = -1
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for negative replicas")
	}
	if !strings.Contains(err.Error(), "replicas must be >= 1") {
		t.Errorf("error should mention replicas >= 1: %v", err)
	}
}

func TestLoad_ReplicasTooLarge(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_a.toml"), `
command = ["x"]
replicas = 1000
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for too-large replicas")
	}
	if !strings.Contains(err.Error(), "replicas must be <= 64") {
		t.Errorf("error should mention <= 64: %v", err)
	}
}

func TestLoad_ReservedReplicaIndexInServiceEnv(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_a.toml"), `
command = ["x"]
[env]
ZPINIT_REPLICA_INDEX = "0"
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for reserved env key in service [env]")
	}
	if !strings.Contains(err.Error(), "reserved") || !strings.Contains(err.Error(), "ZPINIT_REPLICA_INDEX") {
		t.Errorf("error should mention reserved + ZPINIT_REPLICA_INDEX: %v", err)
	}
}

func TestLoad_ReservedReplicaIndexInGlobalsEnv(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `
[env]
ZPINIT_REPLICA_INDEX = "0"
`)
	write(t, filepath.Join(dir, "services", "10_a.toml"), `command = ["x"]`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for reserved env key in globals [env]")
	}
	if !strings.Contains(err.Error(), "reserved") || !strings.Contains(err.Error(), "ZPINIT_REPLICA_INDEX") {
		t.Errorf("error should mention reserved + ZPINIT_REPLICA_INDEX: %v", err)
	}
}

func TestLoad_ExitCodeFromReplicatedConflict(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `exit_code_from = "worker"`)
	write(t, filepath.Join(dir, "services", "10_worker.toml"), `
command = ["x"]
replicas = 3
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for exit_code_from + replicas")
	}
	if !strings.Contains(err.Error(), "replicated service") {
		t.Errorf("error should mention replicated service: %v", err)
	}
}

func TestLoad_ExitCodeFromSingleReplicaOK(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "zpinit.toml"), `exit_code_from = "worker"`)
	write(t, filepath.Join(dir, "services", "10_worker.toml"), `
command = ["x"]
replicas = 1
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// Load returns an error wrapping fs.ErrNotExist when the dir is
// missing. Callers (notably cmd/zpinit/main.go) detect this with
// errors.Is to permit wrap-mode-with-no-config; this test pins the
// contract so a future refactor can't silently break that path.
func TestLoad_MissingDirIsErrNotExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	_, err := Load(missing)
	if err == nil {
		t.Fatal("expected error for missing config dir")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error should wrap fs.ErrNotExist, got: %v", err)
	}
}

// NewEmpty produces a Config that supervise mode would refuse (zero
// services) but wrap mode runs fine on. Defaults must be populated
// because runEntrypoint and the orchestrator both read them.
func TestNewEmpty_AppliesDefaults(t *testing.T) {
	cfg := NewEmpty("/etc/zpinit")
	if cfg == nil {
		t.Fatal("NewEmpty returned nil")
	}
	if cfg.Dir != "/etc/zpinit" {
		t.Errorf("Dir = %q, want /etc/zpinit", cfg.Dir)
	}
	if len(cfg.Services) != 0 {
		t.Errorf("Services should be empty, got %d", len(cfg.Services))
	}
	if cfg.Globals.EntrypointOnFailure == "" {
		t.Error("EntrypointOnFailure default not applied")
	}
	if cfg.Globals.DefaultStopSignal == "" {
		t.Error("DefaultStopSignal default not applied")
	}
	if cfg.Globals.BootTimeout == 0 {
		t.Error("BootTimeout default not applied")
	}
}
