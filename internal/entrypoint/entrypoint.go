// Package entrypoint runs the /etc/zpinit/entrypoint.d/* phase: a
// sequential series of executable scripts that prepare the container
// before either supervising services or exec'ing a wrapped CMD.
//
// Scripts may export environment to subsequent steps by appending
// KEY=value lines to an env file (typically /run/zpinit/env). The runner
// re-reads that file before each script and once more after the last,
// returning the accumulated env to the caller.
package entrypoint

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

type OnFailure string

const (
	Fail     OnFailure = "fail"
	Continue OnFailure = "continue"
)

type Config struct {
	Dir           string        // entrypoint.d directory
	OnFailure     OnFailure     // fail | continue (default: fail)
	ScriptTimeout time.Duration // 0 disables; default applied by caller
	KillGrace     time.Duration // SIGTERM-to-SIGKILL grace on timeout (default 5s)
	EnvFile       string        // path scripts write KEY=value to (e.g. /run/zpinit/env)
	// InitialEnv is the starting env that scripts inherit and that the
	// returned env layers on top of. nil falls back to os.Environ() for
	// backwards-compat (and tests). Production callers compose
	// container env + globals.Env here so scripts see the merged map.
	InitialEnv map[string]string
	Logger     *slog.Logger
}

// Run executes entrypoint.d/* sequentially. Returns the accumulated env
// (container env merged with everything written to EnvFile) suitable for
// passing to exec'd CMD or supervised services.
func Run(ctx context.Context, cfg Config) (map[string]string, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.OnFailure == "" {
		cfg.OnFailure = Fail
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = 5 * time.Second
	}

	if cfg.EnvFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.EnvFile), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			cfg.Logger.Warn("could not ensure env file directory; env propagation may not work", "path", cfg.EnvFile, "err", err)
		}
	}

	scripts, err := listScripts(cfg.Dir)
	if err != nil {
		return nil, err
	}

	var env map[string]string
	if cfg.InitialEnv != nil {
		env = make(map[string]string, len(cfg.InitialEnv))
		for k, v := range cfg.InitialEnv {
			env[k] = v
		}
	} else {
		env = mapFromEnviron(os.Environ())
	}

	for _, path := range scripts {
		if err := mergeEnvFile(env, cfg.EnvFile, cfg.Logger); err != nil {
			return nil, fmt.Errorf("read env file: %w", err)
		}

		name := filepath.Base(path)
		fmt.Fprintln(os.Stderr, "[zpinit] entrypoint.d:", name)

		if err := runOne(ctx, cfg, path, sliceFromEnviron(env)); err != nil {
			if cfg.OnFailure == Continue {
				cfg.Logger.Warn("entrypoint.d script failed; continuing", "name", name, "err", err)
				fmt.Fprintf(os.Stderr, "[zpinit] entrypoint.d: %s failed: %v (continuing)\n", name, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "[zpinit] entrypoint.d: %s failed: %v\n", name, err)
			return nil, fmt.Errorf("entrypoint.d/%s: %w", name, err)
		}
	}

	if err := mergeEnvFile(env, cfg.EnvFile, cfg.Logger); err != nil {
		return nil, fmt.Errorf("read env file (final): %w", err)
	}
	return env, nil
}

func listScripts(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".disabled") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		if info.Mode()&0o111 == 0 {
			// run-parts(8) convention: silently skip non-executable files.
			// --check-config emits a warning so users notice.
			continue
		}
		out = append(out, filepath.Join(dir, n))
	}
	sort.Strings(out)
	return out, nil
}

func runOne(ctx context.Context, cfg Config, path string, env []string) error {
	cmd := exec.Command(path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	cmd.Env = env
	// Setpgid puts the script in its own process group so SIGTERM/SIGKILL
	// reaches grandchildren too. Without this, a script that forks
	// (e.g. wraps `composer install`) would leave its children running
	// after timeout/cancel — they get reparented to PID 1 (us) and
	// continue to consume resources into the supervise phase.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	pgid := cmd.Process.Pid

	// cmd.Wait() runs in its own goroutine. If the kernel can't deliver
	// SIGKILL (process pinned in uninterruptible sleep — e.g. wedged
	// NFS), Wait blocks forever and this goroutine leaks. Buffered chan
	// (cap 1) so the eventual send succeeds even if no one reads.
	// Acceptable: entrypoint runs once at boot before the centralized
	// reaper exists, so we can't route through SpawnTracked here.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timeoutCh <-chan time.Time
	if cfg.ScriptTimeout > 0 {
		t := time.NewTimer(cfg.ScriptTimeout)
		defer t.Stop()
		timeoutCh = t.C
	}

	select {
	case err := <-done:
		return err
	case <-timeoutCh:
		cfg.Logger.Warn("entrypoint script timed out", "name", filepath.Base(path), "timeout", cfg.ScriptTimeout)
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}

	graceTimer := time.NewTimer(cfg.KillGrace)
	defer graceTimer.Stop()
	select {
	case err := <-done:
		return err
	case <-graceTimer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
		return errors.New("script killed after timeout / cancellation")
	}
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// mergeEnvFile applies KEY=value lines from path into env. Empty lines and
// lines starting with # are ignored. Malformed lines are logged at warn
// level and skipped — never fatal, since a typo'd line shouldn't abort
// the boot.
func mergeEnvFile(env map[string]string, path string, log *slog.Logger) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineno := 0
	for scanner.Scan() {
		lineno++
		raw := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		i := strings.IndexByte(raw, '=')
		if i <= 0 {
			log.Warn("malformed env line; skipping", "file", path, "line", lineno, "content", raw)
			continue
		}
		key := raw[:i]
		if !envKeyPattern.MatchString(key) {
			log.Warn("invalid env key; skipping", "file", path, "line", lineno, "key", key)
			continue
		}
		env[key] = raw[i+1:]
	}
	return scanner.Err()
}

func mapFromEnviron(envv []string) map[string]string {
	m := make(map[string]string, len(envv))
	for _, e := range envv {
		if i := strings.IndexByte(e, '='); i > 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

// SliceFromEnviron converts a key→value map to KEY=value form for exec.Env
// or syscall.Exec. Exported because callers (e.g. the wrap-mode dispatch)
// need it after Run returns the merged env.
func SliceFromEnviron(m map[string]string) []string {
	return sliceFromEnviron(m)
}

func sliceFromEnviron(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
