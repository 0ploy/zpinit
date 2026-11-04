// Package execpath resolves a command name against a given PATH, the
// way exec.LookPath does against the process's own. zpinit resolves a
// child's argv[0] against the child's env (the merged env of the wrap
// CMD, an exec session's env), and PID 1 must not rewrite its own PATH
// to do it: in the resident wrap and in exec sessions zpinit keeps
// running and would keep the workload's PATH.
package execpath

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Find resolves name against path (a PATH value). A name with a slash
// is checked as is. Errors are *exec.Error, matching exec.LookPath:
// exec.ErrNotFound when nothing matches, and exec.ErrDot (with the
// path) when the match came from a relative PATH entry.
func Find(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		if err := executable(name); err != nil {
			return "", &exec.Error{Name: name, Err: err}
		}
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if executable(p) == nil {
			if !filepath.IsAbs(p) {
				return p, &exec.Error{Name: name, Err: exec.ErrDot}
			}
			return p, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// FromEnv is the PATH in a KEY=VALUE env slice (the last one wins, as
// getenv sees it), and whether there is one.
func FromEnv(env []string) (string, bool) {
	path, ok := "", false
	for _, kv := range env {
		if v, found := strings.CutPrefix(kv, "PATH="); found {
			path, ok = v, true
		}
	}
	return path, ok
}

func executable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	switch m := fi.Mode(); {
	case m.IsDir():
		return syscall.EISDIR
	case m&0o111 == 0:
		return fs.ErrPermission
	}
	return nil
}
