// Package service spawns one supervised process with the right
// SysProcAttr (own process group, parent-death signal on Linux),
// optional uid/gid credentials, and stdout/stderr destinations.
//
// Spawn registers the child with the centralized reaper atomically;
// the resulting Process exposes an Exit channel that fires once with
// the ExitInfo when the kernel reaps the child. Phase 4 wraps this
// in per-service goroutines with restart logic.
package service

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/reaper"
)

type Process struct {
	Name      string
	PID       int
	StartedAt time.Time
	Exit      <-chan reaper.ExitInfo
}

// Spawn starts cfg's command with the merged environment and registers
// it with the reaper. Returns once the process is running (or the start
// syscall has failed); the caller should select on Process.Exit to learn
// when the process dies.
func Spawn(cfg config.Service, baseEnv []string, r *reaper.Reaper, log *slog.Logger) (*Process, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("empty command")
	}

	cred, err := resolveCredentials(cfg.User, cfg.Group)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}

	stdoutFile, stdoutTarget, err := openLogTarget(cfg.Log.Stdout, os.Stdout)
	if err != nil {
		return nil, fmt.Errorf("stdout: %w", err)
	}
	stderrFile, stderrTarget, err := openLogTarget(cfg.Log.Stderr, os.Stderr)
	if err != nil {
		closeFile(stdoutFile)
		return nil, fmt.Errorf("stderr: %w", err)
	}

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.Cwd
	cmd.Env = MergeEnv(baseEnv, cfg.Env)
	cmd.Stdout = stdoutTarget
	cmd.Stderr = stderrTarget
	cmd.SysProcAttr = baseSysProcAttr()
	if cred != nil {
		cmd.SysProcAttr.Credential = cred
	}

	proc, exitCh, spawnErr := r.SpawnTracked(func() (*os.Process, error) {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	})

	// We always close the parent's copies — the kernel duplicated the
	// fds for the child during fork+exec and the child's are independent.
	closeFile(stdoutFile)
	closeFile(stderrFile)

	if spawnErr != nil {
		return nil, fmt.Errorf("start: %w", spawnErr)
	}

	// Defensive: any future error between here and successful return
	// must Untrack the PID, otherwise the reaper map keeps the entry
	// (and its 1-buffered channel) alive until the OS eventually
	// delivers SIGCHLD. The release at the end of the happy path
	// hands ownership of the channel to the returned Process, which
	// the supervisor consumes — so the bookkeeping is balanced.
	released := false
	defer func() {
		if !released {
			r.Untrack(proc.Pid)
		}
	}()

	log.Info("spawned", "service", cfg.Name, "pid", proc.Pid, "cmd", cfg.Command)

	released = true
	return &Process{
		Name:      cfg.Name,
		PID:       proc.Pid,
		StartedAt: time.Now(),
		Exit:      exitCh,
	}, nil
}

// SignalGroup sends sig to the entire process group, reaching forks
// and double-forks of the service (e.g. php-fpm workers).
func (p *Process) SignalGroup(sig syscall.Signal) error {
	return syscall.Kill(-p.PID, sig)
}

// SpawnOneShot runs a transient command (a service's reload_command,
// say) under the centralized reaper and returns a channel that fires
// with the ExitInfo once the kernel reaps the child. The caller is
// expected to read from the channel exactly once. Stdout/stderr
// inherit zpinit's own; the operator sees the output in the
// supervisor log.
//
// Unlike Spawn this does not create a long-lived Process record
// because there is no per-service state machine to drive; the
// caller drives it directly.
func SpawnOneShot(name string, command, env []string, r *reaper.Reaper, log *slog.Logger) (int, <-chan reaper.ExitInfo, error) {
	if len(command) == 0 {
		return 0, nil, errors.New("empty command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = baseSysProcAttr()

	proc, exitCh, err := r.SpawnTracked(func() (*os.Process, error) {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	})
	if err != nil {
		return 0, nil, fmt.Errorf("start: %w", err)
	}
	log.Info("one-shot spawned", "service", name, "pid", proc.Pid, "cmd", command)
	return proc.Pid, exitCh, nil
}

func resolveCredentials(userStr, groupStr string) (*syscall.Credential, error) {
	if userStr == "" && groupStr == "" {
		return nil, nil
	}

	var uid, gid uint32

	if userStr != "" {
		u, err := lookupUser(userStr)
		if err != nil {
			return nil, fmt.Errorf("user %q: %w", userStr, err)
		}
		uid64, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("user %q: parse uid %q: %w", userStr, u.Uid, err)
		}
		uid = uint32(uid64)
		gid64, _ := strconv.ParseUint(u.Gid, 10, 32)
		gid = uint32(gid64)
	}

	if groupStr != "" {
		g, err := lookupGroup(groupStr)
		if err != nil {
			return nil, fmt.Errorf("group %q: %w", groupStr, err)
		}
		gid64, err := strconv.ParseUint(g.Gid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("group %q: parse gid %q: %w", groupStr, g.Gid, err)
		}
		gid = uint32(gid64)
	}

	return &syscall.Credential{Uid: uid, Gid: gid}, nil
}

func lookupUser(s string) (*user.User, error) {
	if isNumeric(s) {
		return user.LookupId(s)
	}
	return user.Lookup(s)
}

func lookupGroup(s string) (*user.Group, error) {
	if isNumeric(s) {
		return user.LookupGroupId(s)
	}
	return user.LookupGroup(s)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// openLogTarget interprets a [log].stdout/stderr value:
//
//	"" or "inherit" -> use inheritFrom (typically os.Stdout/os.Stderr)
//	absolute path   -> open with O_APPEND|O_CREATE|O_WRONLY|O_NOFOLLOW, mode 0o644
//
// O_NOFOLLOW rejects the open if the *final* path component is a
// symlink. Without it, an attacker (or careless config) that put a
// symlink at the configured log path would have zpinit append the
// service's stdout to the symlink target — which could be any file
// the daemon's UID can write. Symlinked parent directories are still
// followed normally; only the leaf is protected, matching standard
// log-writer hardening.
//
// The parent directory of the log path is created with MkdirAll
// (mode 0o755) before the open. zpinit only ever mkdirs paths the
// operator explicitly named in [log], and the symlink-leaf check
// happens unchanged on the OpenFile call below, so the security
// guarantee is preserved. This removes the per-image
// `entrypoint.d/00-mklogdir.sh` boilerplate.
//
// Returns (file, target). The caller passes target to cmd.Stdout/Stderr
// and must Close file (if non-nil) after Start; the kernel duplicates
// fds for the child so the parent's copy is no longer needed.
func openLogTarget(spec string, inheritFrom *os.File) (*os.File, *os.File, error) {
	if spec == "" || spec == "inherit" {
		return nil, inheritFrom, nil
	}
	if !filepath.IsAbs(spec) {
		return nil, nil, fmt.Errorf("log path must be absolute: %s", spec)
	}
	if err := os.MkdirAll(filepath.Dir(spec), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(spec, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, f, nil
}

func closeFile(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

// MergeEnv returns base env with [env] overrides applied. New entries
// are appended; existing keys are replaced in place. Used at spawn
// time and by the supervisor for readiness probes.
func MergeEnv(base []string, override map[string]string) []string {
	if len(override) == 0 {
		return base
	}
	seen := make(map[string]bool, len(override))
	out := make([]string, 0, len(base)+len(override))
	for _, e := range base {
		if i := strings.IndexByte(e, '='); i > 0 {
			k := e[:i]
			if v, ok := override[k]; ok {
				out = append(out, k+"="+v)
				seen[k] = true
				continue
			}
		}
		out = append(out, e)
	}
	for k, v := range override {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}
