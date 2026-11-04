//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a bytes.Buffer safe to read while exec's copier
// goroutine writes it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startZpinitBG starts zpinit in the background and returns the command
// plus its combined output. The caller drives it with signals.
func startZpinitBG(t *testing.T, env []string, args ...string) (*exec.Cmd, *lockedBuffer) {
	t.Helper()
	cmd := exec.Command(zpinitBin, args...)
	cmd.Env = append(os.Environ(), env...)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	cmd.Stdout = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd, stderr
}

// waitExit waits for cmd and returns its exit code.
func waitExit(t *testing.T, cmd *exec.Cmd, stderr *lockedBuffer, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCodeOf(err)
	case <-time.After(timeout):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-done
		t.Fatalf("zpinit did not exit within %v; output:\n%s", timeout, stderr.String())
		return -1
	}
}

// strayFatalSignals are signals the Go runtime would end zpinit on
// without a Notify (HUP; QUIT and the fault signals when sent by kill),
// plus a sample of those it would ignore anyway.
var strayFatalSignals = []syscall.Signal{
	syscall.SIGILL, syscall.SIGTRAP, syscall.SIGABRT, syscall.SIGBUS, syscall.SIGFPE,
	syscall.SIGSEGV, syscall.SIGSYS, syscall.SIGALRM, syscall.SIGUSR1, syscall.SIGUSR2,
	syscall.SIGPIPE, syscall.SIGVTALRM, syscall.SIGPROF, syscall.SIGXCPU, syscall.SIGXFSZ,
	syscall.SIGIO, syscall.Signal(40),
}

func TestSurvivesStraySignals(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("signal set is Linux's")
	}
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `command = ["sleep", "60"]`)
	cmd, stderr := startZpinitBG(t, []string{"ZPINIT_ENV_FILE=" + filepath.Join(t.TempDir(), "env")}, "--config", cfg)
	if !waitOutput(stderr, "zpinit started", 5*time.Second) {
		t.Fatalf("zpinit never started:\n%s", stderr.String())
	}
	sigs := append([]syscall.Signal{syscall.SIGSTKFLT, syscall.SIGPWR}, strayFatalSignals...)
	for _, s := range sigs {
		_ = cmd.Process.Signal(s)
	}
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("zpinit died on a stray signal: %v\n%s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), "goroutine ") {
		t.Errorf("stray signal produced a goroutine dump:\n%s", stderr.String())
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if code := waitExit(t, cmd, stderr, 15*time.Second); code != 0 {
		t.Errorf("exit = %d after SIGTERM, want 0:\n%s", code, stderr.String())
	}
}

func waitOutput(buf *lockedBuffer, substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestInitWrapExitStatus(t *testing.T) {
	cfg := t.TempDir()
	r := runZpinit(t, nil, "--init", "--config", cfg, "--", "sh", "-c", "exit 7")
	if r.exitCode != 7 {
		t.Errorf("exit = %d, want 7\n%s", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stderr, "init=fork") {
		t.Errorf("banner does not show the resident wrap:\n%s", r.stderr)
	}
	// ZPINIT_INIT=1 is the env spelling; a signal death is 128+N.
	r = runZpinit(t, map[string]string{"ZPINIT_INIT": "1"}, "--config", cfg, "--", "sh", "-c", "kill -SEGV $$")
	if r.exitCode != 128+int(syscall.SIGSEGV) {
		t.Errorf("exit = %d, want %d\n%s", r.exitCode, 128+int(syscall.SIGSEGV), r.stderr)
	}
}

func TestInitWrapStaysResident(t *testing.T) {
	cfg := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd, stderr := startZpinitBG(t, nil, "--init", "--config", cfg, "--", "sh", "-c", "echo $$ > "+pidFile+"; exec sleep 60")
	if _, ok := waitForFile(pidFile, 5*time.Second); !ok {
		t.Fatalf("CMD never ran:\n%s", stderr.String())
	}
	b, _ := os.ReadFile(pidFile)
	var child int
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &child)
	if child == 0 || child == cmd.Process.Pid {
		t.Fatalf("CMD pid %d; zpinit pid %d: the CMD must be a child, not exec'd over zpinit", child, cmd.Process.Pid)
	}
	// SIGTERM is forwarded to the CMD's group; zpinit exits 128+15.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if code := waitExit(t, cmd, stderr, 10*time.Second); code != 143 {
		t.Errorf("exit = %d, want 143:\n%s", code, stderr.String())
	}
}

func TestInitWrapForwardsSignals(t *testing.T) {
	cfg := t.TempDir()
	marker := filepath.Join(t.TempDir(), "got")
	ready := filepath.Join(t.TempDir(), "ready")
	script := fmt.Sprintf(`trap 'echo usr1 > %s; exit 0' USR1; touch %s; while :; do sleep 0.05; done`, marker, ready)
	cmd, stderr := startZpinitBG(t, nil, "--init", "--config", cfg, "--", "sh", "-c", script)
	if _, ok := waitForFile(ready, 5*time.Second); !ok {
		t.Fatalf("CMD never ran:\n%s", stderr.String())
	}
	_ = cmd.Process.Signal(syscall.SIGUSR1)
	if code := waitExit(t, cmd, stderr, 10*time.Second); code != 0 {
		t.Errorf("exit = %d, want 0:\n%s", code, stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("SIGUSR1 never reached the CMD:\n%s", stderr.String())
	}
}

func TestInitWrapStopTimeoutKills(t *testing.T) {
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "zpinit.toml"), `default_stop_timeout = "500ms"`)
	ready := filepath.Join(t.TempDir(), "ready")
	script := fmt.Sprintf(`trap '' TERM; touch %s; while :; do sleep 0.05; done`, ready)
	cmd, stderr := startZpinitBG(t, nil, "--init", "--config", cfg, "--", "sh", "-c", script)
	if _, ok := waitForFile(ready, 5*time.Second); !ok {
		t.Fatalf("CMD never ran:\n%s", stderr.String())
	}
	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if code := waitExit(t, cmd, stderr, 10*time.Second); code != 137 {
		t.Errorf("exit = %d, want 137 (SIGKILL after the stop timeout):\n%s", code, stderr.String())
	}
	if el := time.Since(start); el < 400*time.Millisecond || el > 5*time.Second {
		t.Errorf("stop took %v, want about the 500ms stop timeout", el)
	}
}

// TestContainerModeAsPID1 runs zpinit as PID 1 in a container with a
// planted machine.json: it must report container context, ignore the
// file (so no vsock listener and no reboot), reap orphans in --init wrap mode, and exit with the CMD's status.
func TestContainerModeAsPID1(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("docker PID 1 test is Linux-only")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v\n%s", err, out)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "zpinit")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/zpinit")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	planted := filepath.Join(dir, "machine.json")
	writeFile(t, planted, `{"v":1,"console":"/dev/console","stop":{"signal":"SIGTERM","timeout":"1s"},"exec":{"port":1024}}`)

	name := fmt.Sprintf("zpinit-it-init-%d-%d", os.Getpid(), time.Now().UnixNano())
	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = docker("rm", "-f", name) })

	// The CMD orphans a short-lived grandchild, waits for the test's go
	// signal, and exits 5.
	script := `(sleep 0.3 &); while [ ! -e /tmp/go ]; do sleep 0.05; done; exit 5`
	if out, err := docker("run", "-d", "--name", name,
		"-v", bin+":/usr/local/bin/zpinit:ro",
		"-v", planted+":/run/zpinit/machine.json:ro",
		"alpine:3.20", "zpinit", "--init", "--", "sh", "-c", script); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	logs := func() string {
		out, _ := docker("logs", name)
		return out
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs(), "reaped orphan") {
		time.Sleep(100 * time.Millisecond)
	}
	l := logs()
	for _, want := range []string{"context=container", "ignoring it", "reaped orphan", "init=fork"} {
		if !strings.Contains(l, want) {
			t.Errorf("logs lack %q:\n%s", want, l)
		}
	}
	for _, unwanted := range []string{"exec listener", "context=machine", "ending the machine"} {
		if strings.Contains(l, unwanted) {
			t.Errorf("logs contain %q in a container:\n%s", unwanted, l)
		}
	}
	if out, err := docker("exec", name, "touch", "/tmp/go"); err != nil {
		t.Fatalf("docker exec: %v\n%s", err, out)
	}
	if out, err := docker("wait", name); err != nil || strings.TrimSpace(out) != "5" {
		t.Errorf("container exit = %q (%v), want 5; logs:\n%s", strings.TrimSpace(out), err, logs())
	}
}

// TestContainerModeSupervisePID1SurvivesSignals: zpinit as a
// container's PID 1 in supervise mode does not die of signals the Go
// runtime would otherwise treat as fatal.
func TestContainerModeSupervisePID1SurvivesSignals(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("docker PID 1 test is Linux-only")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v\n%s", err, out)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "zpinit")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/zpinit")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cfg := filepath.Join(dir, "etc")
	writeFile(t, filepath.Join(cfg, "services", "10_app.toml"), `command = ["sleep", "600"]`)
	if err := os.Chmod(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("zpinit-it-sig-%d-%d", os.Getpid(), time.Now().UnixNano())
	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = docker("rm", "-f", name) })
	if out, err := docker("run", "-d", "--name", name,
		"-v", bin+":/usr/local/bin/zpinit:ro", "-v", cfg+":/etc/zpinit:ro",
		"alpine:3.20", "zpinit"); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := docker("logs", name); strings.Contains(out, "zpinit started") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, s := range []string{"ALRM", "USR1", "PWR", "SEGV", "ILL", "TRAP", "ABRT", "SYS", "STKFLT", "VTALRM", "RTMIN+5"} {
		// From inside the container, as a stray workload would.
		_, _ = docker("exec", name, "kill", "-s", s, "1")
	}
	time.Sleep(300 * time.Millisecond)
	if out, _ := docker("inspect", "-f", "{{.State.Running}}", name); strings.TrimSpace(out) != "true" {
		out, _ := docker("logs", name)
		t.Fatalf("PID 1 died on a stray signal; logs:\n%s", out)
	}
	if out, err := docker("stop", "-t", "10", name); err != nil {
		t.Fatalf("docker stop: %v\n%s", err, out)
	}
	if out, _ := docker("inspect", "-f", "{{.State.ExitCode}}", name); strings.TrimSpace(out) != "0" {
		logs, _ := docker("logs", name)
		t.Errorf("exit code = %q, want 0; logs:\n%s", strings.TrimSpace(out), logs)
	}
}
