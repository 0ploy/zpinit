package config

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLoad_PdeathsigAndCrashBudget(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_default.toml"), `command = ["a"]`)
	write(t, filepath.Join(dir, "services", "20_shim.toml"), `
command = ["shim"]
pdeathsig = false
max_consecutive_crashes = 0
`)
	write(t, filepath.Join(dir, "services", "30_strict.toml"), `
command = ["b"]
pdeathsig = true
max_consecutive_crashes = 2
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 3 {
		t.Fatalf("services = %d, skipped = %v", len(cfg.Services), cfg.SkippedFiles)
	}
	def, shim, strict := cfg.Services[0], cfg.Services[1], cfg.Services[2]
	if !def.WantsPdeathsig() || def.CrashBudget() != DefaultMaxConsecutiveCrashes {
		t.Errorf("default: pdeathsig=%v budget=%d", def.WantsPdeathsig(), def.CrashBudget())
	}
	if shim.WantsPdeathsig() || shim.CrashBudget() != 0 {
		t.Errorf("shim: pdeathsig=%v budget=%d", shim.WantsPdeathsig(), shim.CrashBudget())
	}
	if !strict.WantsPdeathsig() || strict.CrashBudget() != 2 {
		t.Errorf("strict: pdeathsig=%v budget=%d", strict.WantsPdeathsig(), strict.CrashBudget())
	}
}

func TestLoad_NegativeCrashBudgetRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "services", "10_x.toml"), "command = [\"x\"]\nmax_consecutive_crashes = -1\n")
	if msg := loadExpectSkip(t, dir); !strings.Contains(msg, "max_consecutive_crashes") {
		t.Errorf("error = %q", msg)
	}
}

func TestSignalName(t *testing.T) {
	cases := map[syscall.Signal]string{
		syscall.SIGTERM:    "SIGTERM",
		syscall.SIGSEGV:    "SIGSEGV",
		syscall.SIGKILL:    "SIGKILL",
		syscall.Signal(40): "SIG40",
	}
	for sig, want := range cases {
		if got := SignalName(sig); got != want {
			t.Errorf("SignalName(%d) = %q, want %q", sig, got, want)
		}
	}
	// Every name round-trips through ParseSignal.
	for n, sig := range signalsByName {
		got, ok := ParseSignal("SIG" + n)
		if !ok || got != sig || SignalName(sig) != "SIG"+n {
			t.Errorf("round trip %s: %v %v %q", n, got, ok, SignalName(sig))
		}
	}
}
