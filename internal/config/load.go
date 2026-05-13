package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/0ploy/zpinit/internal/resources"
)

const (
	globalsFile   = "zpinit.toml"
	servicesDir   = "services"
	entrypointDir = "entrypoint.d"
)

var (
	namePattern   = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	prefixStrip   = regexp.MustCompile(`^\d+[-_]?`)
	envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Load reads zpinit.toml and services/*.toml from dir, applies defaults,
// validates everything, and returns a fully-populated Config. On validation
// failure it returns an error that wraps every problem found, so several
// mistakes can be fixed in one cycle instead of one per run.
//
// If dir doesn't exist the returned error wraps fs.ErrNotExist, so callers
// that want to permit a missing default config dir (wrap-mode-with-no-config)
// can detect it via errors.Is and fall back to NewEmpty.
func Load(dir string) (*Config, error) {
	cfg := &Config{Dir: dir}

	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("config dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", dir)
	}

	if err := loadGlobals(filepath.Join(dir, globalsFile), &cfg.Globals); err != nil {
		return nil, err
	}
	applyGlobalDefaults(&cfg.Globals)

	if err := loadServices(filepath.Join(dir, servicesDir), cfg); err != nil {
		return nil, err
	}

	if err := scanEntrypoint(filepath.Join(dir, entrypointDir), cfg); err != nil {
		return nil, err
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// NewEmpty returns a Config with global defaults applied and no
// services or entrypoint scripts. Used when the config dir is missing
// at the default path: zpinit can still wrap a CMD with sensible
// defaults rather than refusing to start, mirroring tini's behavior of
// "give me a CMD and I'll just exec it." Operators who pass --config
// explicitly still get a hard error if the path is missing, since that
// is unambiguously a typo or mount mistake.
func NewEmpty(dir string) *Config {
	cfg := &Config{Dir: dir}
	applyGlobalDefaults(&cfg.Globals)
	return cfg
}

func loadGlobals(path string, g *Globals) error {
	md, err := toml.DecodeFile(path, g)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return fmt.Errorf("%s: unknown keys: %s", path, joinKeys(undecoded))
	}
	return nil
}

func applyGlobalDefaults(g *Globals) {
	if g.EntrypointOnFailure == "" {
		g.EntrypointOnFailure = EntrypointFail
	}
	if g.EntrypointScriptTimeout == 0 {
		g.EntrypointScriptTimeout = Duration(5 * time.Minute)
	}
	if g.BootTimeout == 0 {
		g.BootTimeout = Duration(60 * time.Second)
	}
	if g.DefaultStopSignal == "" {
		g.DefaultStopSignal = "TERM"
	}
	if g.DefaultStopTimeout == 0 {
		g.DefaultStopTimeout = Duration(10 * time.Second)
	}
	if g.ExitCodeFrom == "" {
		g.ExitCodeFrom = "default"
	}
	if g.ControlSocket == "" {
		g.ControlSocket = "/run/zpinit.sock"
	}
}

func loadServices(dir string, cfg *Config) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}
	// os.ReadDir is documented as sorted by name on most systems, but be explicit:
	// the boot order is load-bearing and we don't want it to depend on the OS.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		var svc Service
		md, err := toml.DecodeFile(path, &svc)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			return fmt.Errorf("%s: unknown keys: %s", path, joinKeys(undecoded))
		}
		svc.Filename = e.Name()
		if svc.Name == "" {
			svc.Name = nameFromFilename(e.Name())
		}
		applyServiceDefaults(&svc, &cfg.Globals)
		cfg.Services = append(cfg.Services, svc)
	}
	return nil
}

// nameFromFilename strips a leading numeric prefix (with optional - or _
// separator) and the .toml suffix.
//
//	10_mysql.toml -> "mysql"
//	30-nginx.toml -> "nginx"
//	cron.toml     -> "cron"
//	99_worker.toml -> "worker"
func nameFromFilename(filename string) string {
	base := strings.TrimSuffix(filename, ".toml")
	return prefixStrip.ReplaceAllString(base, "")
}

// MaxReplicas caps Replicas. The cap prevents accidental fork-bombs
// from typos (replicas = 1000); 64 is generous for any real workload
// zpinit targets. Promote to a config knob only if anyone asks.
const MaxReplicas = 64

// reservedReplicaIndexKey is owned by the supervisor: it gets injected
// into each replica's env in `expandServiceToRunners` and must not be
// overridden by globals or per-service `[env]`. Validation rejects it
// in either place so operators don't silently shadow the supervisor's
// per-replica value via the env-merge precedence chain.
const reservedReplicaIndexKey = "ZPINIT_REPLICA_INDEX"

// reservedEnvKeys collects every env key zpinit injects into a
// child's environment. Operator [env] tables (globals or per-service)
// may not set these: the supervisor's value would lose the merge race
// in some code paths and win in others, depending on layer order, so
// allowing user overrides makes behavior position-dependent and
// confusing. Validation rejects them outright.
var reservedEnvKeys = []string{
	reservedReplicaIndexKey,
	resources.EnvCPUCount,
	resources.EnvCPUQuota,
	resources.EnvMemoryBytes,
}

func isReservedEnvKey(k string) bool {
	for _, r := range reservedEnvKeys {
		if k == r {
			return true
		}
	}
	return false
}

func applyServiceDefaults(s *Service, g *Globals) {
	if s.Replicas == 0 {
		s.Replicas = 1
	}
	if s.Restart == "" {
		s.Restart = RestartAlways
	}
	if s.BackoffInitial == 0 {
		s.BackoffInitial = Duration(1 * time.Second)
	}
	if s.BackoffMax == 0 {
		s.BackoffMax = Duration(30 * time.Second)
	}
	if s.BackoffResetAfter == 0 {
		s.BackoffResetAfter = Duration(60 * time.Second)
	}
	if s.StopSignal == "" {
		s.StopSignal = g.DefaultStopSignal
	}
	if s.StopTimeout == 0 {
		s.StopTimeout = g.DefaultStopTimeout
	}
	if s.Log.Stdout == "" {
		s.Log.Stdout = "inherit"
	}
	if s.Log.Stderr == "" {
		s.Log.Stderr = "inherit"
	}
	if s.Ready != nil {
		if s.Ready.Interval == 0 {
			s.Ready.Interval = Duration(500 * time.Millisecond)
		}
		if s.Ready.Timeout == 0 {
			s.Ready.Timeout = Duration(30 * time.Second)
		}
		if s.Ready.OnTimeout == "" {
			s.Ready.OnTimeout = ReadyFail
		}
	}
}

// scanEntrypoint inspects entrypoint.d/ and warns about non-executable files;
// the file list itself is regenerated at run time, but we want to surface
// likely mistakes during --check-config.
func scanEntrypoint(dir string, cfg *Config) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".disabled") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", filepath.Join(dir, name), err)
		}
		if info.Mode()&0o111 == 0 {
			cfg.Warnings = append(cfg.Warnings,
				fmt.Sprintf("entrypoint.d/%s is not executable; will be skipped at runtime", name))
		}
	}
	return nil
}

func validate(cfg *Config) error {
	var errs []string

	switch cfg.Globals.EntrypointOnFailure {
	case EntrypointFail, EntrypointContinue:
	default:
		errs = append(errs, fmt.Sprintf("entrypoint_on_failure must be 'fail' or 'continue', got %q", cfg.Globals.EntrypointOnFailure))
	}
	if _, ok := ParseSignal(cfg.Globals.DefaultStopSignal); !ok {
		errs = append(errs, fmt.Sprintf("default_stop_signal %q is not a recognised signal name", cfg.Globals.DefaultStopSignal))
	}
	if !filepath.IsAbs(cfg.Globals.ControlSocket) {
		errs = append(errs, fmt.Sprintf("control_socket %q must be an absolute path", cfg.Globals.ControlSocket))
	}
	for k := range cfg.Globals.Env {
		if !envKeyPattern.MatchString(k) {
			errs = append(errs, fmt.Sprintf("env key %q must match %s", k, envKeyPattern))
		}
		if isReservedEnvKey(k) {
			errs = append(errs, fmt.Sprintf("env key %q is reserved (managed by the supervisor)", k))
		}
	}
	if cfg.Globals.Resources.ReserveCPU < 0 {
		errs = append(errs, fmt.Sprintf("[resources].reserve_cpu must be >= 0, got %v", cfg.Globals.Resources.ReserveCPU))
	}

	nameToFile := map[string]string{}
	for i := range cfg.Services {
		s := &cfg.Services[i]
		if !namePattern.MatchString(s.Name) {
			errs = append(errs, fmt.Sprintf("%s: name %q must match %s", s.Filename, s.Name, namePattern))
		}
		if other, ok := nameToFile[s.Name]; ok {
			errs = append(errs, fmt.Sprintf("name collision: %s and %s both resolve to %q", other, s.Filename, s.Name))
		} else {
			nameToFile[s.Name] = s.Filename
		}
		if len(s.Command) == 0 {
			errs = append(errs, fmt.Sprintf("%s: command is required", s.Filename))
		}
		switch s.Restart {
		case RestartAlways, RestartOnFailure, RestartNever:
		default:
			errs = append(errs, fmt.Sprintf("%s: restart must be 'always', 'on-failure', or 'never'; got %q", s.Filename, s.Restart))
		}
		if _, ok := ParseSignal(s.StopSignal); !ok {
			errs = append(errs, fmt.Sprintf("%s: stop_signal %q is not a recognised signal name", s.Filename, s.StopSignal))
		}
		if s.Replicas < 1 {
			errs = append(errs, fmt.Sprintf("%s: replicas must be >= 1", s.Filename))
		}
		if s.Replicas > MaxReplicas {
			errs = append(errs, fmt.Sprintf("%s: replicas must be <= %d (got %d)", s.Filename, MaxReplicas, s.Replicas))
		}
		for k := range s.Env {
			if !envKeyPattern.MatchString(k) {
				errs = append(errs, fmt.Sprintf("%s: env key %q must match %s", s.Filename, k, envKeyPattern))
			}
			if isReservedEnvKey(k) {
				errs = append(errs, fmt.Sprintf("%s: env key %q is reserved (managed by the supervisor)", s.Filename, k))
			}
		}
		if s.Ready != nil {
			if len(s.Ready.Command) == 0 {
				errs = append(errs, fmt.Sprintf("%s: [ready].command is required when [ready] is set", s.Filename))
			}
			switch s.Ready.OnTimeout {
			case ReadyFail, ReadyContinue:
			default:
				errs = append(errs, fmt.Sprintf("%s: [ready].on_timeout must be 'fail' or 'continue'; got %q", s.Filename, s.Ready.OnTimeout))
			}
		}
	}

	if cfg.Globals.ExitCodeFrom != "default" {
		if _, ok := nameToFile[cfg.Globals.ExitCodeFrom]; !ok {
			errs = append(errs, fmt.Sprintf("exit_code_from = %q references unknown service", cfg.Globals.ExitCodeFrom))
		} else {
			for _, s := range cfg.Services {
				if s.Name == cfg.Globals.ExitCodeFrom && s.Replicas > 1 {
					errs = append(errs, fmt.Sprintf("exit_code_from = %q references a replicated service (replicas=%d); the combination is ambiguous", cfg.Globals.ExitCodeFrom, s.Replicas))
					break
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func joinKeys(keys []toml.Key) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k.String()
	}
	return strings.Join(parts, ", ")
}
