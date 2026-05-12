// Package doctor implements `zpinit doctor`: a read-only environment
// audit that catches the misconfigurations zpinit would otherwise
// only discover at boot.
//
// The checks are grouped into four categories: filesystem (paths,
// permissions, writability), config (TOML parse/validate, command
// resolution), runtimes (Node/Bun/Deno version compatibility for
// replica clustering), and state (whether a zpinit instance is
// already running, env-file freshness).
//
// Run is pure: it does not start services, write files, or modify
// state. cmd/zpinit's printer renders the result and chooses an exit
// code: 0 on all OK (warnings allowed), 1 on any fail, 2 on warnings
// only.
package doctor

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0ploy/zpinit/internal/config"
)

// Status is the verdict of one Check.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusWarn:
		return "WARN"
	case StatusFail:
		return "FAIL"
	}
	return "?"
}

// Check is one finding. Category groups checks for printing; Name is
// a short identifier; Detail is a one-line human-readable description
// (may include path snippets and version strings).
type Check struct {
	Category string
	Name     string
	Status   Status
	Detail   string
}

// Node version floor for replica clustering: server.listen({reusePort:
// true}) was added in 22.12.0 LTS (PR #55408, 2024-12-03). Below
// that, listen() ignores the unknown option and the second replica
// gets EADDRINUSE.
const (
	NodeMinMajor = 22
	NodeMinMinor = 12
	NodeMinPatch = 0
)

// Run executes every check and returns the result list in the order
// they were run. Caller (cmd/zpinit) does the printing and exit-code
// translation.
func Run(configDir string) []Check {
	var checks []Check
	checks = append(checks, checkFilesystem(configDir)...)
	cfg, cfgChecks := checkConfig(configDir)
	checks = append(checks, cfgChecks...)
	if cfg != nil {
		checks = append(checks, checkRuntimes(cfg)...)
		checks = append(checks, checkState(cfg)...)
	}
	return checks
}

func checkFilesystem(configDir string) []Check {
	var out []Check
	add := func(c Check) { out = append(out, c) }

	info, err := os.Stat(configDir)
	switch {
	case err != nil:
		add(Check{"filesystem", "config dir", StatusFail, fmt.Sprintf("%s: %v", configDir, err)})
		return out
	case !info.IsDir():
		add(Check{"filesystem", "config dir", StatusFail, fmt.Sprintf("%s is not a directory", configDir)})
		return out
	default:
		add(Check{"filesystem", "config dir", StatusOK, fmt.Sprintf("%s exists and is a directory", configDir)})
	}

	servicesDir := filepath.Join(configDir, "services")
	if entries, err := os.ReadDir(servicesDir); err != nil {
		if os.IsNotExist(err) {
			add(Check{"filesystem", "services dir", StatusWarn, fmt.Sprintf("%s missing (no services to supervise)", servicesDir)})
		} else {
			add(Check{"filesystem", "services dir", StatusFail, fmt.Sprintf("%s: %v", servicesDir, err)})
		}
	} else {
		add(Check{"filesystem", "services dir", StatusOK, fmt.Sprintf("%s contains %d file(s)", servicesDir, len(entries))})
	}

	entrypointDir := filepath.Join(configDir, "entrypoint.d")
	if entries, err := os.ReadDir(entrypointDir); err != nil {
		if !os.IsNotExist(err) {
			add(Check{"filesystem", "entrypoint.d", StatusFail, fmt.Sprintf("%s: %v", entrypointDir, err)})
		}
		// Missing entrypoint.d is fine; don't even warn.
	} else if len(entries) == 0 {
		add(Check{"filesystem", "entrypoint.d", StatusOK, fmt.Sprintf("%s is empty (no boot scripts)", entrypointDir)})
	} else {
		add(Check{"filesystem", "entrypoint.d", StatusOK, fmt.Sprintf("%s contains %d entry/ies", entrypointDir, len(entries))})
	}

	return out
}

// checkConfig parses and validates the config, returning the loaded
// Config (or nil if loading failed) plus the corresponding Check rows.
// Returning the loaded config lets downstream categories (runtimes,
// state) inspect it without re-parsing.
func checkConfig(configDir string) (*config.Config, []Check) {
	var out []Check
	cfg, err := config.Load(configDir)
	if err != nil {
		out = append(out, Check{"config", "parse + validate", StatusFail, err.Error()})
		return nil, out
	}
	out = append(out, Check{"config", "parse + validate", StatusOK, fmt.Sprintf("%d service(s) parse cleanly", len(cfg.Services))})
	for _, w := range cfg.Warnings {
		out = append(out, Check{"config", "load warning", StatusWarn, w})
	}

	for _, s := range cfg.Services {
		// Replicas: report the log layout (shared file vs per-replica
		// via the {index} placeholder) so the operator can confirm
		// what they get before boot.
		if s.Replicas > 1 && s.Log.Stdout != "" && s.Log.Stdout != "inherit" {
			if strings.Contains(s.Log.Stdout, "{index}") {
				var preview []string
				for i := 0; i < s.Replicas; i++ {
					preview = append(preview, replicaLogPreview(s.Log.Stdout, i, s.Replicas))
				}
				out = append(out, Check{"config", s.Name + ": log paths", StatusOK,
					fmt.Sprintf("replicas=%d, log.stdout expands to: %s", s.Replicas, strings.Join(preview, ", "))})
			} else {
				out = append(out, Check{"config", s.Name + ": log paths", StatusOK,
					fmt.Sprintf("replicas=%d, all replicas share %s (use {index} for per-replica files)", s.Replicas, s.Log.Stdout)})
			}
		}
		if len(s.Command) == 0 {
			continue // already caught by validate
		}
		out = append(out, commandCheck(s.Name, "command", s.Command[0]))
		if s.Ready != nil && len(s.Ready.Command) > 0 {
			out = append(out, commandCheck(s.Name, "[ready].command", s.Ready.Command[0]))
		}
	}
	return cfg, out
}

func commandCheck(svcName, field, cmd string) Check {
	if filepath.IsAbs(cmd) {
		info, err := os.Stat(cmd)
		if err != nil {
			return Check{"config", svcName + ": " + field, StatusFail, fmt.Sprintf("%s not found", cmd)}
		}
		if !info.Mode().IsRegular() {
			return Check{"config", svcName + ": " + field, StatusFail, fmt.Sprintf("%s is not a regular file", cmd)}
		}
		if info.Mode()&0o111 == 0 {
			return Check{"config", svcName + ": " + field, StatusFail, fmt.Sprintf("%s is not executable", cmd)}
		}
		return Check{"config", svcName + ": " + field, StatusOK, fmt.Sprintf("%s is executable", cmd)}
	}
	path, err := exec.LookPath(cmd)
	if err != nil {
		return Check{"config", svcName + ": " + field, StatusFail, fmt.Sprintf("%s not found on PATH", cmd)}
	}
	return Check{"config", svcName + ": " + field, StatusOK, fmt.Sprintf("%s found on PATH (%s)", cmd, path)}
}

// replicaLogPreview expands a {index} placeholder for one replica.
// Callers handle the shared-path case (no placeholder) before reaching
// this helper, so the preview only needs to render the per-replica
// opt-in form.
func replicaLogPreview(spec string, idx, total int) string {
	if total <= 1 || spec == "" || spec == "inherit" {
		return spec
	}
	return strings.ReplaceAll(spec, "{index}", strconv.Itoa(idx))
}

func checkRuntimes(cfg *config.Config) []Check {
	var out []Check
	// Aggregate per-runtime: which services reference node, bun, deno?
	// We do this on cfg.Services rather than walking PATH because the
	// only runtimes we need to inspect are those the services declare.
	usesNode, usesBun, usesDeno := false, false, false
	nodeReplicas := 0
	for _, s := range cfg.Services {
		base := filepath.Base(commandBin(s.Command))
		switch base {
		case "node", "nodejs":
			usesNode = true
			if s.Replicas > 1 {
				nodeReplicas++
			}
		case "bun":
			usesBun = true
		case "deno":
			usesDeno = true
		}
	}

	if usesNode {
		out = append(out, nodeRuntimeCheck(nodeReplicas))
	}
	if usesBun {
		out = append(out, runtimeVersionCheck("bun", []string{"--version"}))
	}
	if usesDeno {
		out = append(out, runtimeVersionCheck("deno", []string{"--version"}))
	}
	return out
}

func commandBin(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	return cmd[0]
}

func nodeRuntimeCheck(nodeReplicas int) Check {
	path, err := exec.LookPath("node")
	if err != nil {
		path, err = exec.LookPath("nodejs")
		if err != nil {
			return Check{"runtimes", "node", StatusFail, "node binary not found on PATH"}
		}
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return Check{"runtimes", "node", StatusWarn, fmt.Sprintf("`%s --version` failed: %v", path, err)}
	}
	maj, min, patch, perr := parseNodeVersion(string(out))
	if perr != nil {
		return Check{"runtimes", "node", StatusWarn, fmt.Sprintf("could not parse version %q: %v", strings.TrimSpace(string(out)), perr)}
	}
	if versionAtLeast(maj, min, patch, NodeMinMajor, NodeMinMinor, NodeMinPatch) {
		return Check{"runtimes", "node", StatusOK,
			fmt.Sprintf("%d.%d.%d supports reusePort (>= %d.%d.%d)", maj, min, patch, NodeMinMajor, NodeMinMinor, NodeMinPatch)}
	}
	// Below the floor: warn only if any node service has replicas > 1.
	if nodeReplicas == 0 {
		return Check{"runtimes", "node", StatusOK,
			fmt.Sprintf("%d.%d.%d detected; clustering would require >= %d.%d.%d (no replicated node services)", maj, min, patch, NodeMinMajor, NodeMinMinor, NodeMinPatch)}
	}
	return Check{"runtimes", "node", StatusWarn,
		fmt.Sprintf("%d.%d.%d detected; %d node service(s) have replicas > 1, but reusePort needs node >= %d.%d.%d; port-binding replicas will collide on EADDRINUSE",
			maj, min, patch, nodeReplicas, NodeMinMajor, NodeMinMinor, NodeMinPatch)}
}

func runtimeVersionCheck(bin string, args []string) Check {
	path, err := exec.LookPath(bin)
	if err != nil {
		return Check{"runtimes", bin, StatusFail, bin + " binary not found on PATH"}
	}
	out, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		return Check{"runtimes", bin, StatusWarn, fmt.Sprintf("`%s %s` failed: %v", path, strings.Join(args, " "), err)}
	}
	return Check{"runtimes", bin, StatusOK, fmt.Sprintf("%s: %s", path, strings.TrimSpace(string(out)))}
}

var nodeVersionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// parseNodeVersion extracts the major.minor.patch numbers from `node
// --version` output. Accepts "v22.12.0", "22.12.0\n", and tolerates
// prerelease suffixes like "v22.13.0-rc.1" by stopping at the patch
// number. Returns an error on unparseable strings (custom forks,
// empty output) — the caller renders WARN, not FAIL, since the
// service may still work; we just can't confirm the version is high
// enough for clustering.
func parseNodeVersion(s string) (maj, min, patch int, err error) {
	m := nodeVersionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, 0, fmt.Errorf("unparseable: %q", strings.TrimSpace(s))
	}
	maj, _ = strconv.Atoi(m[1])
	min, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	return maj, min, patch, nil
}

// versionAtLeast reports whether (a,b,c) >= (x,y,z) under
// major.minor.patch ordering.
func versionAtLeast(a, b, c, x, y, z int) bool {
	if a != x {
		return a > x
	}
	if b != y {
		return b > y
	}
	return c >= z
}

func checkState(cfg *config.Config) []Check {
	var out []Check
	socket := cfg.Globals.ControlSocket
	conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		out = append(out, Check{"state", "control socket", StatusWarn,
			fmt.Sprintf("a zpinit instance is already running at %s", socket)})
	} else {
		out = append(out, Check{"state", "control socket", StatusOK,
			fmt.Sprintf("no zpinit instance currently running (%s)", socket)})
	}
	return out
}
