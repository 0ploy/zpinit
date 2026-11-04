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
	"runtime/debug"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/entrypoint"
	"github.com/0ploy/zpinit/internal/machine"
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
	// Machine mode is detected, never configured: zpinit is the kernel's
	// own init. Its first acts, before flag parsing or anything that can
	// fail: catch the stop signals (Ctrl+Alt+Del is about to arrive as
	// SIGINT, and an uncaught SIGINT would end PID 1, i.e. panic the
	// kernel), then switch Ctrl+Alt+Del from "reset now" to that SIGINT.
	isMachine := machine.Detect()
	var (
		latch  *stopLatch
		sink   *signalSink
		cadErr error
	)
	if isMachine {
		latch = installStopLatch(true)
		sink = installSignalSink(true)
		cadErr = machine.DisableCAD()
	}

	// The default ExitOnError would os.Exit(2) on a bad flag, which for
	// the kernel's init is a kernel panic. In machine mode a flag error
	// goes through the same ending as everything else (below).
	if isMachine {
		flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	}
	var (
		showVersion    bool
		checkConfig    string
		configDir      string
		skipEntrypoint bool
		initFlag       bool
		doctorFlag     bool
		doctorQuiet    bool
		planFlag       bool
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&checkConfig, "check-config", "", "validate configuration in `dir` and exit")
	flag.StringVar(&configDir, "config", defaultConfigDir, "configuration `dir`")
	flag.BoolVar(&skipEntrypoint, "skip-entrypoint", false, "skip entrypoint.d scripts (useful for `docker run image bash` debug shells)")
	flag.BoolVar(&initFlag, "init", false, "wrap mode: stay resident above the CMD (fork it, reap, forward signals, exit with its status) instead of exec'ing it; also ZPINIT_INIT=1")
	flag.BoolVar(&doctorFlag, "doctor", false, "run the pre-flight environment audit and exit (filesystem, config, runtimes, state)")
	flag.BoolVar(&doctorQuiet, "doctor-quiet", false, "with --doctor: suppress OK rows in the output")
	flag.BoolVar(&planFlag, "plan", false, "print the resolved boot plan and exit (dry-run; no exec, no spawn, no entrypoint.d)")
	flagErr := flag.CommandLine.Parse(os.Args[1:])

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// In machine mode nothing may simply exit: PID 1 returning panics
	// the kernel and loses unsynced writes. Every way out goes through
	// machine.End, which tears down, syncs, writes the reset line and
	// reboots. ms is replaced by the real settings once they are read.
	ms := machine.Defaults()
	exit := func(o machine.Outcome) {
		if isMachine {
			machine.End(log, ms, o)
		}
		os.Exit(o.Code)
	}
	if isMachine {
		// A panic on the main goroutine still ends the machine cleanly.
		// (One on another goroutine cannot be recovered; the kernel
		// then panics, and with panic=1 the VM still ends.)
		defer func() {
			if p := recover(); p != nil {
				log.Error("zpinit panic", "panic", p, "stack", string(debug.Stack()))
				machine.End(log, ms, machine.Outcome{Cause: machine.CauseExit, Code: 2})
			}
		}()
	}

	if flagErr != nil {
		// Only reachable in machine mode; the usage text is already out.
		log.Error("invalid command line", "err", flagErr)
		exit(machine.Outcome{Cause: machine.CauseExit, Code: 2})
		return
	}

	if showVersion {
		fmt.Println(version)
		exit(machine.Outcome{Cause: machine.CauseExit})
		return
	}

	if checkConfig != "" {
		exit(machine.Outcome{Cause: machine.CauseExit, Code: runCheckConfig(checkConfig)})
		return
	}

	if doctorFlag {
		exit(machine.Outcome{Cause: machine.CauseExit, Code: runDoctor(configDir, doctorQuiet)})
		return
	}

	if planFlag {
		exit(machine.Outcome{Cause: machine.CauseExit, Code: runPlan(configDir, flag.Args())})
		return
	}

	// Container mode installs its catchers here, after the one-shot
	// commands above, which stay interruptible with Ctrl+C.
	if !isMachine {
		latch = installStopLatch(false)
		sink = installSignalSink(false)
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

	ctxName := "container"
	if isMachine {
		ctxName = "machine"
	}
	// Being PID 1 only adds orphan reaping; zpinit always reaps its own
	// children. So this is a fact for the log, not a warning.
	log.Info("zpinit starting", "version", version, "pid", os.Getpid(), "context", ctxName)

	opts := runOpts{
		configDir:      configDir,
		configExplicit: configExplicit,
		cmdline:        flag.Args(),
		skipEntrypoint: skipEntrypoint,
		init:           initFlag || os.Getenv("ZPINIT_INIT") == "1",
		latch:          latch,
		sink:           sink,
	}

	if isMachine {
		if cadErr != nil {
			log.Error("could not route Ctrl+Alt+Del to zpinit; a host graceful stop will reset the machine at once", "err", cadErr)
		}
		settings, err := machine.Load(machine.SettingsPath)
		if err != nil {
			log.Error("machine settings unusable; parking the machine", "err", err)
		}
		ms = settings
		opts.machine = ms
		// Machine mode forces the resident wrap: PID 1 must never be
		// replaced by a workload whose exit would panic the kernel.
		opts.init = true
		if ms.Rescue != nil {
			exit(runPark(log, ms, latch))
			return
		}
	} else if machine.WarnIfPresent(machine.SettingsPath) {
		log.Warn("machine settings file present but zpinit is not the kernel's init; ignoring it", "path", machine.SettingsPath)
	}

	o := run(log, opts)
	o.Cause = machine.CauseExit
	if latch.requested.Load() {
		o.Cause = machine.CauseStop
	}
	exit(o)
}

// runOpts is everything run needs from main.
type runOpts struct {
	configDir      string
	configExplicit bool
	cmdline        []string
	skipEntrypoint bool
	// init selects the resident (fork) wrap over exec. Forced in
	// machine mode.
	init  bool
	latch *stopLatch
	sink  *signalSink
	// machine is the machine settings; nil in container mode.
	machine *machine.Settings
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
	if len(cfg.SkippedFiles) > 0 {
		for _, fe := range cfg.SkippedFiles {
			fmt.Fprintf(os.Stderr, "skipped %s: %s\n", fe.File, fe.Err.Error())
		}
		fmt.Fprintf(os.Stderr, "config FAILED: %d service file(s) skipped, %d service(s) valid in %s/services\n",
			len(cfg.SkippedFiles), len(cfg.Services), dir)
		return 1
	}
	fmt.Printf("config OK: %d service(s) in %s/services\n", len(cfg.Services), dir)
	return 0
}

func run(log *slog.Logger, opts runOpts) machine.Outcome {
	configDir, configExplicit, cmdline := opts.configDir, opts.configExplicit, opts.cmdline
	fail := func(code int) machine.Outcome { return machine.Outcome{Code: code} }
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
			return fail(1)
		}
	}
	for _, w := range cfg.Warnings {
		log.Warn("config", "warning", w)
	}
	// Per-file load failures must never crash PID 1: log each skipped
	// service file and boot with the valid ones. An operator fixes the
	// file and `zpctl update`s it back in.
	for _, fe := range cfg.SkippedFiles {
		log.Error("config: service file skipped (failed to parse/validate)", "file", fe.File, "err", fe.Err)
	}

	r := reaper.New(log)

	// Layered env composition: globals.Env (lowest) under container env.
	// entrypoint.d scripts can write further overrides to /run/zpinit/env,
	// which run on top. Container env beats globals.Env so an operator
	// can override a baked-in default via `docker run -e`.
	containerEnv := entrypoint.MapFromEnviron(os.Environ())
	initialEnv := layeredMerge(cfg.Globals.Env, containerEnv)

	finalEnv, err := runEntrypoint(log, configDir, cfg, opts.skipEntrypoint, initialEnv, opts.latch)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return fail(1)
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
	// Resolve `replicas = "auto"` to a concrete N for boot. The
	// watcher's debounced commits will keep it in sync from here on.
	cfg.Services = supervisor.ResolveAutoReplicasAtBoot(cfg.Services, snap)
	log.Info("resources detected",
		"cpu_count", snap.CPUCount,
		"cpu_quota", resourceEnv[resources.EnvCPUQuota],
		"memory_bytes", snap.MemoryBytes,
	)
	if !snap.CgroupResolved && opts.machine == nil {
		// We could not tie this process to its own cgroup, so the
		// numbers above may be the HOST's rather than the container's.
		// (Not in machine mode: there is no host view to confuse with
		// ours, the machine's own vCPUs and RAM are the budget.)
		//
		// Loud because the failure is silent otherwise and skews
		// everything downstream: replicas = "auto" over-scales, and
		// apps sizing heaps or worker pools from ZPINIT_MEMORY_BYTES /
		// ZPINIT_CPU_COUNT get a budget they do not have.
		log.Warn("could not locate this container's own cgroup; CPU/memory figures are UNVERIFIED and may describe the host",
			"hint", "run `zpinit --doctor`; check the cgroup mount and whether /proc/self/cgroup is readable")
	}
	bootEnv := layeredMerge(finalEnv, resourceEnv)

	// Boot banner on zpinit's own stderr, before mode dispatch. One
	// line, key=value, fleet-uniform: every `docker logs` triage starts
	// with "did zpinit even start, and which mode?" and the banner
	// answers both on line 1. Writes to stderr (not stdout) so wrap-
	// mode CMDs that emit structured output on stdout aren't polluted.
	// Operators can opt out with ZPINIT_NO_BANNER=1 for noise-sensitive
	// workloads.
	if os.Getenv("ZPINIT_NO_BANNER") != "1" {
		printBootBanner(cmdline, cfg, snap, opts)
	}

	switch detectMode(cmdline) {
	case modeWrap:
		if opts.init {
			return runWrapInit(log, cfg, cmdline, bootEnv, r, opts)
		}
		// A stop that arrived after entrypoint.d but before the exec
		// would otherwise be lost (the latch caught it, and the exec'd
		// CMD resets the handler). Report it the way dying of it would
		// have.
		if sig, ok := opts.latch.Signal().(syscall.Signal); ok {
			return machine.Outcome{Code: 128 + int(sig), Signal: sig}
		}
		return fail(execCmd(log, cmdline, bootEnv))
	case modeSupervise:
		// scriptEnv is the delta entrypoint.d wrote to /run/zpinit/env
		// (or set on its own children that bubbled up via the env
		// file). Captured here so SIGHUP reloads can recompute the
		// per-service env from the *new* globals.Env without re-running
		// scripts: newBaseEnv = newGlobals.Env + containerEnv +
		// scriptEnv + resourceEnv.
		scriptEnv := envDelta(initialEnv, finalEnv)
		return fail(runSupervise(log, configDir, cfg, bootEnv, containerEnv, scriptEnv, resourceEnv, snap, r, opts))
	}
	return fail(1) // unreachable
}

// printBootBanner emits the one-line "what zpinit is about to do"
// summary on stderr. Format is key=value so `docker logs` consumers
// can grep for it; bytes worth optimizing because every container
// the operator restarts emits one of these.
//
// Mode 1 (wrap) shows argv so the operator confirms which binary
// gets execed; mode 3 (supervise) shows the service count. CPU and
// memory come from the already-reserved snapshot so they reflect
// what services will actually see (matches ZPINIT_CPU_COUNT /
// ZPINIT_MEMORY_BYTES).
func printBootBanner(cmdline []string, cfg *config.Config, snap resources.Snapshot, opts runOpts) {
	mode := "supervise"
	tail := fmt.Sprintf("services=%d", len(cfg.Services))
	if len(cmdline) > 0 {
		mode = "wrap"
		tail = fmt.Sprintf("argv=%q", cmdline)
		if opts.init {
			tail = "init=fork " + tail
		}
	}
	ctxName := "container"
	if opts.machine != nil {
		ctxName = "machine"
	}
	mem := "unlimited"
	if snap.MemoryBytes > 0 {
		mem = formatMemoryBanner(snap.MemoryBytes)
	}
	fmt.Fprintf(os.Stderr, "[zpinit %s] context=%s mode=%s %s cpu=%d memory=%s\n",
		version, ctxName, mode, tail, snap.CPUCount, mem)
}

// formatMemoryBanner renders a byte count for the boot banner.
// Picks a binary unit (MiB/GiB) when the value is a whole multiple,
// otherwise prints two decimals of GiB; falls back to raw bytes for
// values below 1 MiB.
func formatMemoryBanner(n uint64) string {
	const Mi = uint64(1) << 20
	const Gi = uint64(1) << 30
	switch {
	case n >= Gi && n%Gi == 0:
		return fmt.Sprintf("%dGiB", n/Gi)
	case n >= Gi:
		return fmt.Sprintf("%.2fGiB", float64(n)/float64(Gi))
	case n >= Mi && n%Mi == 0:
		return fmt.Sprintf("%dMiB", n/Mi)
	case n >= Mi:
		return fmt.Sprintf("%.2fMiB", float64(n)/float64(Mi))
	}
	return fmt.Sprintf("%dB", n)
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

func runEntrypoint(log *slog.Logger, configDir string, cfg *config.Config, skipFlag bool, initialEnv map[string]string, latch *stopLatch) (map[string]string, error) {
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

	// A stop signal (see stopSignals) aborts the running script.
	ctx, cancel := latch.Context()
	defer cancel()

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
	// Resolve the binary against the MERGED env's PATH, not zpinit's
	// inherited one: an entrypoint.d script that exports PATH via
	// /run/zpinit/env expects it to affect binary resolution, exactly
	// as it will affect the exec'd process. exec.LookPath consults the
	// process env, so install the merged PATH first; the mutation is
	// moot because Exec replaces the image (and on failure we exit).
	if p, ok := env["PATH"]; ok {
		_ = os.Setenv("PATH", p)
	}
	argv0, err := exec.LookPath(cmdline[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "zpinit:", err)
		return 127 // shell convention: command not found
	}
	log.Info("exec", "cmd", cmdline)
	if err := syscall.Exec(argv0, cmdline, entrypoint.SliceFromEnviron(env)); err != nil {
		fmt.Fprintln(os.Stderr, "zpinit: exec:", err)
		return 126 // shell convention: found but not executable
	}
	return 0 // unreachable; Exec replaces the process image
}

// runSupervise is the supervise-mode entry point. It splits signals
// onto separate channels: SIGCHLD goes to a dedicated reaper goroutine
// (startReapLoop) that runs throughout shutdown, the stop signals
// (TERM/INT/QUIT) arrive through the stop latch, and SIGHUP goes to the
// user-signal loop here. The split is load-bearing: if reaping shared a channel
// with shutdown, the SIGTERM handler's wait for orchestrator exit would
// block reading SIGCHLDs, the reaper would stop, and child Exit
// channels would never fire (Phase 5 had this bug for one commit).
//
// SIGHUP triggers a reload via orchestrator.Reload (Phase 7): re-load
// config from disk, diff against the running set, apply add/remove/restart.
func runSupervise(log *slog.Logger, configDir string, cfg *config.Config, env map[string]string, containerEnv, scriptEnv, resourceEnv map[string]string, snap resources.Snapshot, r *reaper.Reaper, opts runOpts) int {
	// Stop signals arrive through opts.latch, installed at startup so
	// none is lost between phases; only SIGHUP (reload) is ours here.
	userCh := make(chan os.Signal, 8)
	signal.Notify(userCh, syscall.SIGHUP)

	stopReap := startReapLoop(log, r)
	cleanup := func() {
		stopReap()
		signal.Stop(userCh)
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
	orch.SetCurrentSnapshot(snap)

	// Resource watcher: re-detects on a poll loop, debounces per
	// dimension, and forwards each committed delta to the
	// orchestrator. Lifetime tied to the supervise loop; cancel of
	// watcherCtx stops the polling goroutine.
	//
	// Started only when some service actually consumes a delta
	// (Config.NeedsResourceWatch). The subscription and the dispatch
	// goroutine below are set up either way: an unstarted watcher
	// publishes nothing, so the goroutine parks on an empty channel at
	// no cost, and a later lazy Start needs no extra wiring.
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()
	watcher := resources.NewWatcher(
		cfg.Globals.Resources.ReserveCPU,
		cfg.Globals.Resources.ReserveMemory.Bytes(),
		cfg.Globals.Resources.ScaleUpAfter.Std(),
		cfg.Globals.Resources.ScaleDownAfter.Std(),
		log,
	)
	watcher.SetPollInterval(cfg.Globals.Resources.PollInterval.Std())
	sub, unsubscribe := watcher.Subscribe()
	defer unsubscribe()
	if cfg.NeedsResourceWatch() {
		watcher.Start(watcherCtx)
	} else {
		// Nothing consumes resource deltas in this container. Polling
		// anyway would cost every container sharing the host a wakeup
		// per second to track a cgroup quota that changes about never;
		// with a few hundred containers on one node that is the single
		// largest thing zpinit does at idle. The trade is that the
		// ZPINIT_CPU_* / ZPINIT_MEMORY_BYTES values stay at their
		// boot-time detection: a service that wants them refreshed on
		// its next respawn opts in with reload_on_change.
		log.Info("resource watcher not started; no service uses replicas=\"auto\" or reload_on_change",
			"effect", "ZPINIT_CPU_COUNT/ZPINIT_CPU_QUOTA/ZPINIT_MEMORY_BYTES stay at boot-time values")
	}
	// A reload can introduce the first auto / reload_on_change service
	// into a container that booted without one, so re-evaluate on every
	// committed config. Watcher.Start is idempotent and reports whether
	// this call is the one that started polling. Deliberately one-way:
	// a reload that removes the last such service leaves the watcher
	// running rather than growing a teardown path, which would have to
	// build a fresh Watcher (Start is one-shot) and re-subscribe. That
	// costs exactly what every container paid before this gate existed.
	orch.SetConfigCommitHook(func(needsResourceWatch bool) {
		if !needsResourceWatch {
			return
		}
		if watcher.Start(watcherCtx) {
			log.Info("resource watcher started; a reload added a service that needs live resource detection")
		}
	})
	go func() {
		for {
			select {
			case <-watcherCtx.Done():
				return
			case change := <-sub:
				// Recover per dispatch: a panic in the resource-change
				// path would otherwise kill PID 1 (this goroutine has no
				// other backstop), converting a config hiccup into a
				// container death. Drop the delta and keep watching.
				func() {
					defer func() {
						if p := recover(); p != nil {
							log.Error("resource-change dispatch panic; delta dropped", "panic", p)
						}
					}()
					orch.OnResourceChange(change)
				}()
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
	// `zpctl shutdown` routes through the main loop rather than
	// cancelling ctx directly, so it gets the same treatment a signal
	// does: control socket closed first, teardown bounded by
	// ShutdownBudget. Previously it only cancelled ctx, leaving the
	// socket accepting requests throughout stopAll and the wait for
	// the orchestrator unbounded and unlogged. Buffered + non-blocking
	// so the verb's response is written even if the loop is busy, and
	// a second `zpctl shutdown` during teardown is a no-op.
	shutdownReq := make(chan struct{}, 1)
	ctrl := supervisor.NewControlServer(orch, func() {
		select {
		case shutdownReq <- struct{}{}:
		default:
		}
	}, log)
	go func() {
		if err := ctrl.Listen(ctrlCtx, cfg.Globals.ControlSocket); err != nil {
			log.Error("control socket", "err", err)
		}
	}()

	exitCh := make(chan int, 1)
	go func() { exitCh <- orch.Run(ctx) }()

	if opts.machine != nil {
		startExecServer(ctx, log, opts.machine, r)
		if budget, limit := supervisor.ServicesStopBudget(cfg.Services), opts.machine.Stop.Timeout.Std(); budget > limit {
			log.Warn("the services' worst-case stop exceeds the machine's stop timeout; a graceful stop is cut off at the timeout",
				"services_budget", budget, "stop_timeout", limit)
		}
	}

	log.Info("zpinit started", "version", version, "pid", os.Getpid(), "services", len(cfg.Services))
	if len(cfg.Services) == 0 {
		log.Info("no services configured; control socket up, waiting for reload (zpctl reread / SIGHUP)")
	}

	// beginShutdown is the single teardown path, shared by the signal
	// handler and `zpctl shutdown` so the two cannot drift.
	beginShutdown := func(reason string) int {
		// Recompute the budget against the *current* runner set rather
		// than reusing a boot-time snapshot — reload may have added
		// services or bumped stop_timeout since startup, and the
		// supervisor's outer wait must cover stopAll's serial inner
		// wait.
		budget := orch.ShutdownBudget()
		// In machine mode the whole stop has to fit the machine's stop
		// timeout: the host kills the VMM after its own, longer one,
		// and whatever is still running then is SIGKILLed by
		// machine.End anyway.
		if opts.machine != nil && budget > opts.machine.Stop.Timeout.Std() {
			budget = opts.machine.Stop.Timeout.Std()
		}
		log.Info("shutting down", "reason", reason, "budget", budget)
		// Close the control socket before teardown begins so no new
		// zpctl request lands mid-stopAll. The orchestrator's stopping
		// latch is the second gate for requests already in flight.
		// Closing the listener does not close already-accepted
		// connections, so an in-flight `zpctl shutdown` still receives
		// its response.
		ctrlCancel()
		cancel()
		shutdownTimer := time.NewTimer(budget)
		defer shutdownTimer.Stop()
		// A second shutdown request during this wait is deliberately
		// ignored (nothing reads userCh or shutdownReq anymore):
		// graceful stop is already running at full speed and per-runner
		// SIGKILL escalation is the accelerator. Operators who want an
		// immediate hard kill use the runtime's (docker stop -t 0);
		// Pdeathsig takes the children down with PID 1.
		select {
		case code := <-exitCh:
			cleanup()
			return code
		case <-shutdownTimer.C:
			log.Error("orchestrator did not return within budget; exiting anyway",
				"budget", budget)
			cleanup()
			return 1
		}
	}

	for {
		select {
		case <-opts.latch.Done():
			return beginShutdown("signal " + opts.latch.Signal().String())
		case sig := <-userCh:
			switch sig {
			case syscall.SIGHUP:
				log.Info("SIGHUP: reloading config", "dir", configDir)
				newCfg, err := config.Load(configDir)
				if err != nil {
					log.Error("reload: config load failed; keeping running set", "err", err)
					continue
				}
				for _, fe := range newCfg.SkippedFiles {
					log.Error("reload: service file skipped (failed to parse/validate); keeping any running copy unchanged",
						"file", fe.File, "err", fe.Err)
				}
				// Reload runs inline on the signal loop. Its remove /
				// restart-stop phase is synchronous and bounded by
				// stop_timeout+grace per filename group, so a SIGTERM
				// arriving mid-reload waits in userCh (buffered, not
				// lost) until Reload returns before shutdown begins.
				// Acceptable: the delay is seconds for normal configs and
				// reloadMu would serialize an async reload against zpctl
				// update anyway. If shutdown latency under an
				// orchestrator's kill grace ever bites, move this Reload
				// to its own goroutine, but then the SIGTERM path must
				// wait for the in-flight reload before returning from Run.
				if _, err := orch.Reload(ctx, newCfg); err != nil {
					log.Error("reload: failed", "err", err)
				}
			}
		case <-shutdownReq:
			opts.latch.requested.Store(true)
			return beginShutdown("zpctl shutdown")
		case code := <-exitCh:
			// The orchestrator ended on its own: the exit_code_from
			// target reached a terminal state, or boot failed. Run has
			// already returned, so there is nothing left to bound.
			cleanup()
			return code
		}
	}
}
