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
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/entrypoint"
	"github.com/0ploy/zpinit/internal/reaper"
	"github.com/0ploy/zpinit/internal/supervisor"
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
		return runSupervise(log, configDir, cfg, finalEnv, r)
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

// runSupervise is the supervise-mode entry point. It splits signals
// onto two channels: SIGCHLD goes to a dedicated reaper goroutine that
// runs throughout shutdown, and SIGTERM/INT/HUP go to the user-signal
// loop here. The split is load-bearing — if reaping shared a channel
// with shutdown, the SIGTERM handler's wait for orchestrator exit would
// block reading SIGCHLDs, the reaper would stop, and child Exit
// channels would never fire (Phase 5 had this bug for one commit).
//
// SIGHUP triggers a reload via orchestrator.Reload (Phase 7): re-load
// config from disk, diff against the running set, apply add/remove/restart.
func runSupervise(log *slog.Logger, configDir string, cfg *config.Config, env map[string]string, r *reaper.Reaper) int {
	chldCh := make(chan os.Signal, 16)
	signal.Notify(chldCh, syscall.SIGCHLD)
	userCh := make(chan os.Signal, 8)
	signal.Notify(userCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	reaperStop := make(chan struct{})
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		for {
			select {
			case <-chldCh:
				r.Reap()
			case <-reaperStop:
				r.Reap() // final drain
				return
			}
		}
	}()
	cleanup := func() {
		close(reaperStop)
		<-reaperDone
		signal.Stop(userCh)
		signal.Stop(chldCh)
	}

	envSlice := entrypoint.SliceFromEnviron(env)
	orch := supervisor.NewOrchestrator(cfg, envSlice, r, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exitCh := make(chan int, 1)
	go func() { exitCh <- orch.Run(ctx) }()

	log.Info("zpinit started", "version", version, "pid", os.Getpid(), "services", len(cfg.Services))

	for {
		select {
		case sig := <-userCh:
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				log.Info("shutdown signal", "signal", sig.String())
				cancel()
				select {
				case code := <-exitCh:
					cleanup()
					return code
				case <-time.After(120 * time.Second):
					log.Error("orchestrator did not return within 120s of cancel; exiting anyway")
					cleanup()
					return 1
				}
			case syscall.SIGHUP:
				log.Info("SIGHUP: reloading config", "dir", configDir)
				newCfg, err := config.Load(configDir)
				if err != nil {
					log.Error("reload: config load failed; keeping running set", "err", err)
					continue
				}
				if err := orch.Reload(ctx, newCfg); err != nil {
					log.Error("reload: failed", "err", err)
				}
			}
		case code := <-exitCh:
			cleanup()
			return code
		}
	}
}
