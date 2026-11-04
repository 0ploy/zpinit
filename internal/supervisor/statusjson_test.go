package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/ctlproto"
	"github.com/0ploy/zpinit/internal/reaper"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// decodeStatus parses one NDJSON status line into a generic map so the
// test can assert on presence/absence (null) of each field.
func decodeStatus(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("status line is not valid JSON: %v\nline: %s", err, line)
	}
	return m
}

func TestFormatStatusJSON_RunningSingle(t *testing.T) {
	cfg := config.Service{Name: "api", Filename: "10_api.toml", Replicas: config.Replicas{N: 1}}
	r := NewRunner(cfg, nil, 0, nil, nil, testLog())
	r.state = StateRunning
	r.process = newFakeProcess(4321)
	r.upSince = time.Now().Add(-90 * time.Second)
	r.totalSpawns = 2

	m := decodeStatus(t, formatStatusJSON(r, false))

	if m["name"] != "api" || m["service"] != "api" {
		t.Errorf("name/service = %v/%v", m["name"], m["service"])
	}
	if m["replica_index"] != nil {
		t.Errorf("replica_index should be null for a non-replicated service, got %v", m["replica_index"])
	}
	if m["state"] != "RUNNING" {
		t.Errorf("state = %v, want RUNNING", m["state"])
	}
	if m["pid"] != float64(4321) {
		t.Errorf("pid = %v, want 4321", m["pid"])
	}
	if up, ok := m["uptime_seconds"].(float64); !ok || up < 89 {
		t.Errorf("uptime_seconds = %v, want ~90", m["uptime_seconds"])
	}
	if m["total_spawns"] != float64(2) {
		t.Errorf("total_spawns = %v, want 2", m["total_spawns"])
	}
	// Non-verbose: /proc fields omitted entirely.
	if _, ok := m["rss_bytes"]; ok {
		t.Errorf("rss_bytes should be omitted without --verbose")
	}
}

func TestFormatStatusJSON_ReplicaAndLastExit(t *testing.T) {
	cfg := config.Service{Name: "consumer", Filename: "20_consumer.toml", Replicas: config.Replicas{N: 3}}
	r := NewRunnerForReplica(cfg, cfg, nil, 2, nil, nil, testLog())
	r.state = StateStopped
	r.stoppedManually = false // clean exit -> EXITED
	r.lastExit = reaper.ExitInfo{PID: 77, ExitCode: 5}

	m := decodeStatus(t, formatStatusJSON(r, false))

	if m["name"] != "consumer/2" {
		t.Errorf("name = %v, want consumer/2", m["name"])
	}
	if m["service"] != "consumer" {
		t.Errorf("service = %v, want consumer", m["service"])
	}
	if m["replica_index"] != float64(2) {
		t.Errorf("replica_index = %v, want 2", m["replica_index"])
	}
	if m["state"] != "EXITED" {
		t.Errorf("state = %v, want EXITED", m["state"])
	}
	if m["pid"] != nil {
		t.Errorf("pid should be null when no live process, got %v", m["pid"])
	}
	if m["uptime_seconds"] != nil {
		t.Errorf("uptime_seconds should be null when not running, got %v", m["uptime_seconds"])
	}
	le, ok := m["last_exit"].(map[string]any)
	if !ok {
		t.Fatalf("last_exit = %v, want object", m["last_exit"])
	}
	if le["code"] != float64(5) {
		t.Errorf("last_exit.code = %v, want 5", le["code"])
	}
	if _, hasSig := le["signal"]; hasSig {
		t.Errorf("last_exit should not carry signal for a code exit")
	}
}

func TestFormatStatusJSON_VerboseAddsProcFields(t *testing.T) {
	cfg := config.Service{Name: "api", Filename: "10_api.toml", Replicas: config.Replicas{N: 1}}
	r := NewRunner(cfg, nil, 0, nil, nil, testLog())
	r.state = StateRunning
	// Use our own PID so readProcStats has a real /proc entry on Linux.
	r.process = newFakeProcess(os.Getpid())
	r.upSince = time.Now()

	m := decodeStatus(t, formatStatusJSON(r, true))
	// On Linux these are present (values may be 0 but the keys exist);
	// the non-linux stub returns zeros, but the keys are still emitted
	// because the PID is live.
	for _, k := range []string{"rss_bytes", "cpu_seconds", "fds"} {
		if _, ok := m[k]; !ok {
			t.Errorf("verbose JSON missing %q", k)
		}
	}
}

func TestErrRespFor_MapsUnknownService(t *testing.T) {
	unknown := fmt.Errorf("%w: ghost", errUnknownService)
	if got := errRespFor(unknown); got.Code != ctlproto.CodeUnknownService {
		t.Errorf("unknown service -> code %d, want %d", got.Code, ctlproto.CodeUnknownService)
	}
	if got := errRespFor(errors.New("boom")); got.Code != ctlproto.CodeFailed {
		t.Errorf("generic error -> code %d, want %d", got.Code, ctlproto.CodeFailed)
	}
}

// An OOM kill (machine mode) shows in status: oom_killed in JSON, a
// bare oom_killed marker in the verbose line. Absent otherwise.
func TestStatusReportsOOMKill(t *testing.T) {
	cfg := config.Service{Name: "app", Filename: "10_app.toml"}
	r := NewRunner(cfg, nil, 0, nil, nil, testLog())
	r.state = StateBackoff
	r.lastExit = reaper.ExitInfo{PID: 9, Signaled: true, Signal: syscall.SIGKILL, OOMKilled: true}

	le, ok := decodeStatus(t, formatStatusJSON(r, false))["last_exit"].(map[string]any)
	if !ok || le["oom_killed"] != true || le["signal"] != "killed" {
		t.Errorf("last_exit = %v, want signal killed with oom_killed", le)
	}
	if line := formatStatusLineVerbose(r); !strings.Contains(line, "last_exit=signal:killed oom_killed") {
		t.Errorf("verbose line lacks the OOM marker: %q", line)
	}

	r.lastExit.OOMKilled = false
	le, _ = decodeStatus(t, formatStatusJSON(r, false))["last_exit"].(map[string]any)
	if _, has := le["oom_killed"]; has {
		t.Errorf("oom_killed present for a plain SIGKILL: %v", le)
	}
	if strings.Contains(formatStatusLineVerbose(r), "oom_killed") {
		t.Error("verbose line marks a plain SIGKILL as OOM")
	}
}

// The reset line of a supervise-mode machine reports whether the
// exit_code_from target was OOM-killed.
func TestExitOOMKilled(t *testing.T) {
	svc := config.Service{Name: "worker", Filename: "10_worker.toml"}
	r := NewRunner(svc, nil, 0, nil, nil, testLog())
	r.lastExit = reaper.ExitInfo{PID: 5, Signaled: true, Signal: syscall.SIGKILL, OOMKilled: true}
	o := &Orchestrator{log: testLog(), cfg: &config.Config{Globals: config.Globals{ExitCodeFrom: "worker"}}}
	o.runners = []*Runner{r}
	if !o.ExitOOMKilled() {
		t.Error("ExitOOMKilled = false for an OOM-killed exit_code_from target")
	}
	o.cfg.Globals.ExitCodeFrom = "default"
	if o.ExitOOMKilled() {
		t.Error("ExitOOMKilled = true with exit_code_from = default")
	}
}
