//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every zpctl verb against a live supervisor, in one session, so the
// client/server wire contract (exit codes, bodies, streaming) is pinned
// end-to-end. TestZpctlEndToEnd covers status/stop/pid/shutdown; this
// one covers the rest.
func TestZpctlVerbs(t *testing.T) {
	cfg := t.TempDir()
	socket := filepath.Join(t.TempDir(), "zpinit.sock")
	logDir := t.TempDir()
	alphaLog := filepath.Join(logDir, "alpha.log")
	usrMark := filepath.Join(logDir, "usr1")
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), fmt.Sprintf("control_socket = %q\n", socket))
	// alpha logs to a file (so tail has something to read), reacts to
	// USR1 by writing a marker and to USR2 by logging a line (so
	// signal and tail --follow can be observed).
	writeFile(t, filepath.Join(cfg, "services", "10_alpha.toml"), fmt.Sprintf(`
command = ["/bin/sh", "-c", "trap 'echo got-usr1 >> %s' USR1; trap 'echo follow-me' USR2; echo hello; while :; do sleep 0.05; done"]
restart = "always"
stop_timeout = "1s"
reload_signal = "USR1"

[log]
stdout = %q
`, usrMark, alphaLog))
	writeFile(t, filepath.Join(cfg, "services", "20_beta.toml"), `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"
`)

	zp, stderr := startZpinit(t, cfg, socket)
	exited, exitErr := watchExit(t, zp)
	zpctl := zpctlRunner(t, socket)
	if !waitForRunning(zpctl, "beta", 3*time.Second) {
		t.Fatalf("beta never RUNNING\nstderr:\n%s", stderr.String())
	}

	// Control socket is born 0600 (umask-tightened bind + chmod).
	if fi, err := os.Stat(socket); err != nil {
		t.Errorf("stat socket: %v", err)
	} else if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want socket with 0600 perms", fi.Mode())
	}

	t.Run("help", func(t *testing.T) {
		out, code := zpctl("help")
		if code != 0 || !strings.Contains(out, "signal NAME") || !strings.Contains(out, "Exit codes") {
			t.Errorf("help exit %d:\n%s", code, out)
		}
	})

	t.Run("status verbose and json", func(t *testing.T) {
		out, code := zpctl("status", "--verbose")
		if code != 0 || !strings.Contains(out, "alpha") || !strings.Contains(out, "spawns=1") {
			t.Errorf("status --verbose exit %d:\n%s", code, out)
		}
		out, code = zpctl("status", "--json")
		if code != 0 {
			t.Fatalf("status --json exit %d:\n%s", code, out)
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			var row struct {
				Name  string `json:"name"`
				State string `json:"state"`
				PID   *int   `json:"pid"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("status --json line is not JSON: %q: %v", line, err)
			}
			seen[row.Name] = true
			if row.State != "RUNNING" || row.PID == nil || *row.PID <= 0 {
				t.Errorf("json row %+v: want RUNNING with a pid", row)
			}
		}
		if !seen["alpha"] || !seen["beta"] {
			t.Errorf("json rows = %v, want alpha and beta", seen)
		}
		// Filtering by name.
		out, _ = zpctl("status", "beta")
		if strings.Contains(out, "alpha") {
			t.Errorf("status beta should not list alpha:\n%s", out)
		}
	})

	t.Run("ready", func(t *testing.T) {
		out, code := zpctl("ready")
		if code != 0 || !strings.Contains(out, "all 2 service(s) ready") {
			t.Errorf("ready exit %d:\n%s", code, out)
		}
		if out, code := zpctl("ready", "alpha", "beta"); code != 0 {
			t.Errorf("ready alpha beta exit %d:\n%s", code, out)
		}
	})

	t.Run("tail", func(t *testing.T) {
		if _, ok := waitFileContains(alphaLog, "hello", 2*time.Second); !ok {
			t.Fatalf("alpha never logged hello")
		}
		out, code := zpctl("tail", "alpha")
		if code != 0 || !strings.Contains(out, "hello") {
			t.Errorf("tail alpha exit %d:\n%s", code, out)
		}
		out, code = zpctl("tail", "beta")
		if code != 1 || !strings.Contains(out, "no file to tail") {
			t.Errorf("tail beta (inherit) exit %d, want 1 with explanation:\n%s", code, out)
		}
	})

	t.Run("signal", func(t *testing.T) {
		out, code := zpctl("signal", "alpha", "USR1")
		if code != 0 {
			t.Fatalf("signal exit %d:\n%s", code, out)
		}
		if body, ok := waitFileContains(usrMark, "got-usr1", 2*time.Second); !ok {
			t.Errorf("USR1 never reached alpha; marker = %q", body)
		}
		if out, code := zpctl("signal", "alpha", "BOGUS"); code != 1 || !strings.Contains(out, "unknown signal") {
			t.Errorf("signal BOGUS exit %d:\n%s", code, out)
		}
		if _, code := zpctl("signal", "alpha"); code != 1 {
			t.Errorf("signal without SIG exit %d, want 1 (usage)", code)
		}
	})

	t.Run("reload in place", func(t *testing.T) {
		// reload_signal = USR1: the process keeps its PID and sees USR1.
		before, _ := os.ReadFile(usrMark)
		pidBefore, _ := zpctl("pid", "alpha")
		out, code := zpctl("reload", "alpha")
		if code != 0 || !strings.Contains(out, "alpha: reloaded") {
			t.Fatalf("reload alpha exit %d:\n%s", code, out)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if after, _ := os.ReadFile(usrMark); strings.Count(string(after), "got-usr1") > strings.Count(string(before), "got-usr1") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if after, _ := os.ReadFile(usrMark); strings.Count(string(after), "got-usr1") <= strings.Count(string(before), "got-usr1") {
			t.Errorf("in-place reload did not deliver USR1")
		}
		if pidAfter, _ := zpctl("pid", "alpha"); pidAfter != pidBefore {
			t.Errorf("in-place reload changed the PID: %q -> %q", pidBefore, pidAfter)
		}
	})

	t.Run("tail follow", func(t *testing.T) {
		c := exec.Command(zpctlBin, "--socket", socket, "tail", "--follow", "alpha")
		stdout, err := c.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var errBuf bytes.Buffer
		c.Stderr = &errBuf
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = c.Process.Kill()
			_ = c.Wait()
		}()
		lines := make(chan string, 64)
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		// Give the follower a moment to attach, then make alpha log.
		time.Sleep(200 * time.Millisecond)
		if out, code := zpctl("signal", "alpha", "USR2"); code != 0 {
			t.Fatalf("signal USR2 exit %d:\n%s", code, out)
		}
		deadline := time.After(3 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("follow stream ended before the new line arrived; stderr:\n%s", errBuf.String())
				}
				if strings.Contains(line, "follow-me") {
					return
				}
			case <-deadline:
				t.Fatalf("tail --follow never delivered the new line; stderr:\n%s", errBuf.String())
			}
		}
	})

	t.Run("restart wait", func(t *testing.T) {
		pidBefore, _ := zpctl("pid", "alpha")
		out, code := zpctl("restart", "--wait", "alpha")
		if code != 0 {
			t.Fatalf("restart --wait exit %d:\n%s", code, out)
		}
		if !waitForRunningPIDChange(zpctl, "alpha", strings.TrimSpace(pidBefore), 3*time.Second) {
			t.Errorf("alpha PID unchanged after restart")
		}
	})

	t.Run("reread update resolve", func(t *testing.T) {
		gamma := filepath.Join(cfg, "services", "30_gamma.toml")
		writeFile(t, gamma, `
command = ["/bin/sh", "-c", "sleep 30"]
restart = "always"
stop_timeout = "1s"
`)
		out, code := zpctl("reread")
		if code != 0 || !strings.Contains(out, "+ gamma") {
			t.Fatalf("reread exit %d, want '+ gamma':\n%s", code, out)
		}
		// Dry run: nothing started.
		if st, _ := zpctl("status"); strings.Contains(st, "gamma") {
			t.Errorf("reread started gamma:\n%s", st)
		}
		out, code = zpctl("update")
		if code != 0 {
			t.Fatalf("update exit %d:\n%s", code, out)
		}
		if !waitForRunning(zpctl, "gamma", 3*time.Second) {
			t.Fatalf("gamma never RUNNING after update")
		}
		out, code = zpctl("resolve", "gamma")
		if code != 0 || !strings.Contains(out, "30_gamma.toml") {
			t.Errorf("resolve gamma exit %d:\n%s", code, out)
		}
		// Scoped update: change gamma and alpha on disk, apply only
		// gamma; alpha keeps its PID.
		writeFile(t, gamma, `
command = ["/bin/sh", "-c", "sleep 31"]
restart = "always"
stop_timeout = "1s"
`)
		writeFile(t, filepath.Join(cfg, "services", "20_beta.toml"), `
command = ["/bin/sh", "-c", "sleep 31"]
restart = "always"
stop_timeout = "1s"
`)
		betaPID, _ := zpctl("pid", "beta")
		gammaPID, _ := zpctl("pid", "gamma")
		out, code = zpctl("update", "gamma")
		if code != 0 {
			t.Fatalf("update gamma exit %d:\n%s", code, out)
		}
		if !waitForRunningPIDChange(zpctl, "gamma", strings.TrimSpace(gammaPID), 3*time.Second) {
			t.Errorf("scoped update did not restart gamma")
		}
		if now, _ := zpctl("pid", "beta"); now != betaPID {
			t.Errorf("scoped update to gamma restarted beta: %q -> %q", betaPID, now)
		}
		// A reread now still shows beta pending.
		if out, _ := zpctl("reread"); !strings.Contains(out, "~ beta") {
			t.Errorf("reread should still show beta as changed:\n%s", out)
		}
		// Removing the file and updating stops gamma.
		if err := os.Remove(gamma); err != nil {
			t.Fatal(err)
		}
		if out, code := zpctl("update"); code != 0 {
			t.Fatalf("update after remove exit %d:\n%s", code, out)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if st, _ := zpctl("status"); !strings.Contains(st, "gamma") {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		if st, _ := zpctl("status"); strings.Contains(st, "gamma") {
			t.Errorf("gamma still listed after its file was removed:\n%s", st)
		}
	})

	t.Run("stop start", func(t *testing.T) {
		if out, code := zpctl("stop", "beta"); code != 0 {
			t.Fatalf("stop exit %d:\n%s", code, out)
		}
		if _, ok := waitForStateRow(zpctl, "beta", "STOPPED", 3*time.Second); !ok {
			t.Fatalf("beta never STOPPED")
		}
		if out, code := zpctl("ready"); code != 1 || !strings.Contains(out, "beta: stopped") {
			t.Errorf("ready with a stopped service exit %d:\n%s", code, out)
		}
		if out, code := zpctl("start", "--wait", "beta"); code != 0 {
			t.Fatalf("start --wait exit %d:\n%s", code, out)
		}
		if !waitForRunning(zpctl, "beta", 3*time.Second) {
			t.Errorf("beta not RUNNING after start")
		}
	})

	t.Run("error codes", func(t *testing.T) {
		for _, verb := range [][]string{{"status", "nope"}, {"pid", "nope"}, {"signal", "nope", "HUP"}, {"resolve", "nope"}, {"tail", "nope"}} {
			if out, code := zpctl(verb...); code != 3 {
				t.Errorf("%v exit %d, want 3 (unknown service):\n%s", verb, code, out)
			}
		}
		if _, code := zpctl("frobnicate"); code == 0 {
			t.Errorf("unknown verb exited 0")
		}
		c := exec.Command(zpctlBin, "--socket", filepath.Join(t.TempDir(), "missing.sock"), "status")
		if err := c.Run(); err == nil {
			t.Errorf("missing socket exited 0")
		} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
			t.Errorf("missing socket exit = %v, want 2 (unreachable)", err)
		}
	})

	// shutdown ends PID 1 cleanly.
	if out, code := zpctl("shutdown"); code != 0 {
		t.Fatalf("shutdown exit %d:\n%s", code, out)
	}
	select {
	case <-exited:
		if *exitErr != nil {
			t.Errorf("zpinit exit after shutdown: %v\nstderr:\n%s", *exitErr, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("zpinit did not exit after shutdown\nstderr:\n%s", stderr.String())
	}
}
