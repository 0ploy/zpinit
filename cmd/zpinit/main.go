package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/entrypoint"
	"github.com/0ploy/zpinit/internal/reaper"
)

var version = "dev"

const (
	defaultConfigDir = "/etc/zpinit"
	defaultEnvFile   = "/run/zpinit/env"
)

func main() {
	var (
		showVersion bool
		checkConfig string
		configDir   string
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&checkConfig, "check-config", "", "validate configuration in `dir` and exit")
	flag.StringVar(&configDir, "config", defaultConfigDir, "configuration `dir`")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	if checkConfig != "" {
		os.Exit(runCheckConfig(checkConfig))
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if pid := os.Getpid(); pid != 1 {
		log.Warn("zpinit is not running as PID 1; orphan reaping is unreliable outside containers", "pid", pid)
	}

	os.Exit(run(log, configDir, flag.Args()))
}

func runCheckConfig(dir string) int {
	cfg, err := config.Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	fmt.Printf("config OK: %d service(s) in %s/services\n", len(cfg.Services), dir)
	return 0
}

func run(log *slog.Logger, configDir string, cmdline []string) int {
	cfg, err := config.Load(configDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, w := range cfg.Warnings {
		log.Warn("config", "warning", w)
	}

	r := reaper.New(log)

	finalEnv, err := runEntrypoint(log, configDir, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Drain any zombies produced by entrypoint.d (double-forked daemons,
	// orphans). Doing this before mode dispatch keeps wrap-mode handing a
	// clean slate to the exec'd CMD.
	r.Reap()

	switch detectMode(cmdline, cfg.Services) {
	case modeWrap:
		return execCmd(log, cmdline, finalEnv)
	case modeSupervise:
		log.Info("supervise mode (services start in Phase 3+); idling on signal loop", "service_count", len(cfg.Services))
		return supervisorPlaceholder(log, r)
	case modeError:
		fmt.Fprintf(os.Stderr,
			"zpinit: nothing to do — provide a CMD or populate %s\n",
			filepath.Join(configDir, "services"))
		return 1
	}
	return 1 // unreachable
}

type mode int

const (
	modeWrap mode = iota
	modeSupervise
	modeError
)

func detectMode(cmdline []string, services []config.Service) mode {
	if len(cmdline) > 0 {
		return modeWrap
	}
	if len(services) > 0 {
		return modeSupervise
	}
	return modeError
}

func runEntrypoint(log *slog.Logger, configDir string, cfg *config.Config) (map[string]string, error) {
	if os.Getenv("ZPINIT_SKIP_ENTRYPOINT") == "1" {
		log.Info("ZPINIT_SKIP_ENTRYPOINT=1; skipping entrypoint.d")
		return mapFromEnviron(os.Environ()), nil
	}

	envFile := defaultEnvFile
	// ZPINIT_ENV_FILE is an internal/testing hook that lets integration
	// tests redirect the env file to a writable path. Production runs
	// always see the spec'd /run/zpinit/env.
	if v := os.Getenv("ZPINIT_ENV_FILE"); v != "" {
		envFile = v
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	return entrypoint.Run(ctx, entrypoint.Config{
		Dir:           filepath.Join(configDir, "entrypoint.d"),
		OnFailure:     entrypoint.OnFailure(cfg.Globals.EntrypointOnFailure),
		ScriptTimeout: cfg.Globals.EntrypointScriptTimeout.Std(),
		EnvFile:       envFile,
		Logger:        log,
	})
}

func execCmd(log *slog.Logger, cmdline []string, env map[string]string) int {
	argv0, err := exec.LookPath(cmdline[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "zpinit:", err)
		return 127
	}
	log.Info("exec", "cmd", cmdline)
	if err := syscall.Exec(argv0, cmdline, entrypoint.SliceFromEnviron(env)); err != nil {
		fmt.Fprintln(os.Stderr, "zpinit: exec:", err)
		return 1
	}
	return 0 // unreachable; Exec replaces the process image
}

func mapFromEnviron(envv []string) map[string]string {
	m := make(map[string]string, len(envv))
	for _, e := range envv {
		if i := indexEq(e); i > 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

func indexEq(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return i
		}
	}
	return -1
}

// supervisorPlaceholder keeps zpinit alive in supervise mode until a real
// supervisor lands in Phase 3+. It runs the same signal loop as Phase 1
// so SIGCHLD reaps and SIGTERM exits cleanly — this is what integration
// test #20 ("supervise mode keeps PID 1 alive") exercises.
func supervisorPlaceholder(log *slog.Logger, r *reaper.Reaper) int {
	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	log.Info("zpinit started", "version", version, "pid", os.Getpid())

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGCHLD:
			r.Reap()
		case syscall.SIGTERM, syscall.SIGINT:
			log.Info("shutdown signal received", "signal", sig.String())
			r.Reap()
			return 0
		case syscall.SIGHUP:
			log.Info("SIGHUP received; reload not yet implemented (Phase 7)")
		}
	}
}
