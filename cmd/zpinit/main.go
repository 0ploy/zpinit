package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
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
	"github.com/0ploy/zpinit/internal/resources"
	"github.com/0ploy/zpinit/internal/supervisor"
)

var version = "dev"

const (
	defaultConfigDir = "/etc/zpinit"
	defaultEnvFile   = "/run/zpinit/env"
)

func main() {
	var (
		showVersion    bool
		checkConfig    string
		configDir      string
		skipEntrypoint bool
		doctorFlag     bool
		doctorQuiet    bool
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&checkConfig, "check-config", "", "validate configuration in `dir` and exit")
	flag.StringVar(&configDir, "config", defaultConfigDir, "configuration `dir`")
	flag.BoolVar(&skipEntrypoint, "skip-entrypoint", false, "skip entrypoint.d scripts (useful for `docker run image bash` debug shells)")
	flag.BoolVar(&doctorFlag, "doctor", false, "run the pre-flight environment audit and exit (filesystem, config, runtimes, state)")
	flag.BoolVar(&doctorQuiet, "doctor-quiet", false, "with --doctor: suppress OK rows in the output")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	if checkConfig != "" {
		os.Exit(runCheckConfig(checkConfig))
	}

	if doctorFlag {
		os.Exit(runDoctor(configDir, doctorQuiet))
	}

	// Track whether --config was passed explicitly so missing-dir
	// handling can distinguish "operator gave a wrong path" (hard
	// error) from "default path doesn't exist, just wrap the CMD".
	configExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configExplicit = true
		}
	})

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if pid := os.Getpid(); pid != 1 {
		log.Warn("zpinit is not running as PID 1; orphan reaping is unreliable outside containers", "pid", pid)
	}

	os.Exit(run(log, configDir, configExplicit, flag.Args(), skipEntrypoint))
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

func run(log *slog.Logger, configDir string, configExplicit bool, cmdline []string, skipEntrypoint bool) int {
	// Self-bootstrap the default config dir so a freshly-pulled image
	// (or fresh install) just works: an operator can drop service files
	// into /etc/zpinit/services/ and zpctl-reread them in without first
	// having to mkdir the layout. Skipped when --config was passed
	// explicitly so an operator typo or missing mount still fails loud
	// instead of silently creating an empty directory at the wrong path.
	if !configExplicit {
		if err := os.MkdirAll(filepath.Join(configDir, "services"), 0o755); err != nil {
			log.Warn("could not create services dir", "dir", configDir, "err", err)
		}
		if err := os.MkdirAll(filepath.Join(configDir, "entrypoint.d"), 0o755); err != nil {
			log.Warn("could not create entrypoint.d dir", "dir", configDir, "err", err)
		}
	}

	cfg, err := config.Load(configDir)
	if err != nil {
		// Defense in depth: the auto-mkdir above almost always means
		// the default dir exists by the time we get here, but a
		// readonly rootfs or a failed mkdir can still leave it
		// missing. An explicit --config to a missing path is always a
		// hard error.
		if errors.Is(err, fs.ErrNotExist) && !configExplicit {
			log.Info("no config dir; running with built-in defaults", "dir", configDir)
			cfg = config.NewEmpty(configDir)
		} else {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	for _, w := range cfg.Warnings {
		log.Warn("config", "warning", w)
	}

	r := reaper.New(log)

	// Layered env composition: globals.Env (lowest) under container env.
	// entrypoint.d scripts can write further overrides to /run/zpinit/env,
	// which run on top. Container env beats globals.Env so an operator
	// can override a baked-in default via `docker run -e`.
	containerEnv := entrypoint.MapFromEnviron(os.Environ())
	initialEnv := layeredMerge(cfg.Globals.Env, containerEnv)

	finalEnv, err := runEntrypoint(log, configDir, cfg, skipEntrypoint, initialEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Drain any zombies produced by entrypoint.d (double-forked daemons,
	// orphans). Doing this before mode dispatch keeps wrap-mode handing a
	// clean slate to the exec'd CMD.
	r.Reap()

	// Detect the container's CPU/memory budget once and inject
	// ZPINIT_CPU_COUNT, ZPINIT_CPU_QUOTA, ZPINIT_MEMORY_BYTES into the
	// env that the wrapped CMD or supervised services see. Highest
	// precedence so a stale container/script value can't shadow the
	// detected truth; [env] validation rejects the keys upfront anyway.
	snap := resources.Detect().WithReserves(
		cfg.Globals.Resources.ReserveCPU,
		cfg.Globals.Resources.ReserveMemory.Bytes(),
	)
	resourceEnv := snap.EnvVars()
	log.Info("resources detected",
		"cpu_count", snap.CPUCount,
		"cpu_quota", resourceEnv[resources.EnvCPUQuota],
		"memory_bytes", snap.MemoryBytes,
	)
	bootEnv := layeredMerge(finalEnv, resourceEnv)

	switch detectMode(cmdline) {
	case modeWrap:
		return execCmd(log, cmdline, bootEnv)
	case modeSupervise:
		// scriptEnv is the delta entrypoint.d wrote to /run/zpinit/env
		// (or set on its own children that bubbled up via the env
		// file). Captured here so SIGHUP reloads can recompute the
		// per-service env from the *new* globals.Env without re-running
		// scripts: newBaseEnv = newGlobals.Env + containerEnv +
		// scriptEnv + resourceEnv.
		scriptEnv := envDelta(initialEnv, finalEnv)
		return runSupervise(log, configDir, cfg, bootEnv, containerEnv, scriptEnv, resourceEnv, r)
	}
	return 1 // unreachable
}

// layeredMerge merges maps left-to-right; later maps override earlier ones.
// Used to compose the env precedence chain: globals.Env (lowest), container
// env, scripts (highest).
func layeredMerge(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range layers {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// envDelta returns the keys in final whose value differs from base (or
// that are absent from base). Used to extract the "what scripts added or
// changed" portion of the entrypoint result, so reloads can recompose
// the env from layered sources without re-running scripts.
func envDelta(base, final map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range final {
		if bv, ok := base[k]; !ok || bv != v {
			out[k] = v
		}
	}
	return out
}

type mode int

const (
	modeWrap mode = iota
	modeSupervise
)

// detectMode picks the dispatch path from the CMD alone: any CMD wins
// (wrap), no CMD means supervise — even with zero services, in which
// case the orchestrator boots nothing, the control socket comes up,
// and an operator can add services via zpctl reread or SIGHUP.
func detectMode(cmdline []string) mode {
	if len(cmdline) > 0 {
		return modeWrap
	}
	return modeSupervise
}

func runEntrypoint(log *slog.Logger, configDir string, cfg *config.Config, skipFlag bool, initialEnv map[string]string) (map[string]string, error) {
	if skipFlag {
		log.Info("--skip-entrypoint set; skipping entrypoint.d")
		// Return the layered initial env so globals.Env still reaches
		// the wrapped CMD (and any debug shell) even when scripts are
		// skipped.
		return initialEnv, nil
	}
	if os.Getenv("ZPINIT_SKIP_ENTRYPOINT") == "1" {
		log.Info("ZPINIT_SKIP_ENTRYPOINT=1; skipping entrypoint.d")
		return initialEnv, nil
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
		InitialEnv:    initialEnv,
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
func runSupervise(log *slog.Logger, configDir string, cfg *config.Config, env map[string]string, containerEnv, scriptEnv, resourceEnv map[string]string, r *reaper.Reaper) int {
	chldCh := make(chan os.Signal, 16)
	signal.Notify(chldCh, syscall.SIGCHLD)
	userCh := make(chan os.Signal, 8)
	signal.Notify(userCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	reaperStop := make(chan struct{})
	reaperDone := make(chan struct{})
	// Reap is wrapped with recover() so an unforeseen panic doesn't
	// take the entire SIGCHLD handler offline (which would silently
	// wedge every Runner waiting on its child's Exit channel). The
	// recover survives, the loop continues, and the next SIGCHLD
	// retries.
	safeReap := func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("reaper panic; continuing", "panic", p)
			}
		}()
		r.Reap()
	}
	go func() {
		defer close(reaperDone)
		for {
			select {
			case <-chldCh:
				safeReap()
			case <-reaperStop:
				safeReap() // final drain
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
	// On reload, recompose the per-service base env from the *new*
	// globals.Env layered with the boot-time container env, boot-time
	// script deltas, and the *current* resource snapshot. Scripts only
	// run once at boot, so reload can't re-derive their additions;
	// capturing scriptEnv preserves them. resourceEnv flows through
	// the orchestrator's SetResourceEnv path, called once at boot here
	// and again from the watcher on every committed delta.
	orch.SetBaseEnvBuilder(func(globalsEnv, currentResourceEnv map[string]string) []string {
		merged := layeredMerge(globalsEnv, containerEnv, scriptEnv, currentResourceEnv)
		return entrypoint.SliceFromEnviron(merged)
	})
	orch.SetResourceEnv(resourceEnv)

	// Resource watcher: re-detects on a poll loop, debounces per
	// dimension, and forwards each committed delta to the
	// orchestrator. Lifetime tied to the supervise loop; cancel of
	// watcherCtx stops the polling goroutine.
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()
	watcher := resources.NewWatcher(
		cfg.Globals.Resources.ReserveCPU,
		cfg.Globals.Resources.ReserveMemory.Bytes(),
		cfg.Globals.Resources.ScaleUpAfter.Std(),
		cfg.Globals.Resources.ScaleDownAfter.Std(),
		log,
	)
	sub := watcher.Subscribe()
	watcher.Start(watcherCtx)
	go func() {
		for {
			select {
			case <-watcherCtx.Done():
				return
			case change := <-sub:
				orch.OnResourceChange(change)
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Control socket: zpctl talks to us over /run/zpinit.sock (or
	// whatever the config sets). The shutdown verb cancels ctx, which
	// propagates to the orchestrator just like SIGTERM would.
	ctrlCtx, ctrlCancel := context.WithCancel(context.Background())
	defer ctrlCancel()
	ctrl := supervisor.NewControlServer(orch, cancel, log)
	go func() {
		if err := ctrl.Listen(ctrlCtx, cfg.Globals.ControlSocket); err != nil {
			log.Error("control socket", "err", err)
		}
	}()

	exitCh := make(chan int, 1)
	go func() { exitCh <- orch.Run(ctx) }()

	log.Info("zpinit started", "version", version, "pid", os.Getpid(), "services", len(cfg.Services))
	if len(cfg.Services) == 0 {
		log.Info("no services configured; control socket up, waiting for reload (zpctl reread / SIGHUP)")
	}

	for {
		select {
		case sig := <-userCh:
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				// Recompute the budget against the *current* runner
				// set rather than reusing a boot-time snapshot —
				// reload may have added services or bumped
				// stop_timeout since startup, and the supervisor's
				// outer wait must cover stopAll's serial inner wait.
				budget := orch.ShutdownBudget()
				log.Info("shutdown signal", "signal", sig.String(), "budget", budget)
				cancel()
				shutdownTimer := time.NewTimer(budget)
				select {
				case code := <-exitCh:
					shutdownTimer.Stop()
					cleanup()
					return code
				case <-shutdownTimer.C:
					log.Error("orchestrator did not return within budget; exiting anyway",
						"budget", budget)
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
