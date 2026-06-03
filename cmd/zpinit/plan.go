package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/0ploy/zpinit/internal/config"
	"github.com/0ploy/zpinit/internal/resources"
	"github.com/0ploy/zpinit/internal/supervisor"
)

// runPlan implements `zpinit --plan`: a dry-run that loads config,
// detects resources, resolves `replicas = "auto"` against the
// current snapshot, expands per-replica log paths, and prints the
// resolved boot plan that would have been used. No exec, no spawn,
// no entrypoint.d execution; safe to run anywhere zpinit can read
// the config dir.
//
// Output is human-readable and intentionally informal: CI scripts
// can diff it across image versions to catch unexpected boot-plan
// changes, but it's not a contract. Exit codes mirror
// --check-config: 0 on a parseable plan, 1 on a config error.
func runPlan(configDir string, cmdline []string) int {
	return printPlan(os.Stdout, configDir, cmdline)
}

func printPlan(w io.Writer, configDir string, cmdline []string) int {
	cfg, err := config.Load(configDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, warn := range cfg.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warn)
	}

	snap := resources.Detect().WithReserves(
		cfg.Globals.Resources.ReserveCPU,
		cfg.Globals.Resources.ReserveMemory.Bytes(),
	)
	cfg.Services = supervisor.ResolveAutoReplicasAtBoot(cfg.Services, snap)

	mode := "supervise"
	if len(cmdline) > 0 {
		mode = "wrap"
	}
	fmt.Fprintf(w, "zpinit %s plan: %s\n\n", version, configDir)
	fmt.Fprintln(w, "resources:")
	fmt.Fprintf(w, "  cpu_count    %d\n", snap.CPUCount)
	fmt.Fprintf(w, "  cpu_quota    %s\n", snap.EnvVars()[resources.EnvCPUQuota])
	if snap.MemoryBytes > 0 {
		fmt.Fprintf(w, "  memory_bytes %d (%s)\n", snap.MemoryBytes, formatMemoryBanner(snap.MemoryBytes))
	} else {
		fmt.Fprintf(w, "  memory_bytes 0 (unlimited or unknown)\n")
	}
	if cfg.Globals.Resources.ReserveCPU > 0 || cfg.Globals.Resources.ReserveMemory.Bytes() > 0 {
		fmt.Fprintf(w, "  reservations cpu=%g memory=%d\n",
			cfg.Globals.Resources.ReserveCPU,
			cfg.Globals.Resources.ReserveMemory.Bytes())
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "env (globals):")
	printSortedMap(w, "  ", cfg.Globals.Env)
	fmt.Fprintln(w, "env (injected by zpinit):")
	printSortedMap(w, "  ", snap.EnvVars())
	fmt.Fprintln(w)

	fmt.Fprintln(w, "entrypoint.d:")
	if scripts := listEntrypointScripts(configDir); len(scripts) > 0 {
		for _, s := range scripts {
			fmt.Fprintf(w, "  %s\n", s)
		}
	} else {
		fmt.Fprintln(w, "  (none)")
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "mode: %s\n", mode)
	if mode == "wrap" {
		fmt.Fprintf(w, "  argv  %q\n", cmdline)
		fmt.Fprintln(w, "  (services/ is ignored when a CMD is supplied)")
		return 0
	}

	fmt.Fprintf(w, "services (%d, in filename order):\n", len(cfg.Services))
	if len(cfg.Services) == 0 {
		fmt.Fprintln(w, "  (none; control socket would come up empty)")
	}
	for _, s := range cfg.Services {
		printServicePlan(w, s)
	}
	if cfg.Globals.ExitCodeFrom != "default" {
		fmt.Fprintf(w, "exit_code_from: %s\n", cfg.Globals.ExitCodeFrom)
	}
	return 0
}

// printServicePlan renders one service's resolved plan. For
// replicated services it expands every per-replica log path so the
// operator sees exactly which files would be opened.
func printServicePlan(w io.Writer, s config.Service) {
	fmt.Fprintf(w, "  %s  (file %s)\n", s.Name, s.Filename)
	fmt.Fprintf(w, "    command   %q\n", s.Command)
	if s.Cwd != "" {
		fmt.Fprintf(w, "    cwd       %s\n", s.Cwd)
	}
	if s.User != "" || s.Group != "" {
		fmt.Fprintf(w, "    user      %s:%s\n", s.User, s.Group)
	}
	fmt.Fprintf(w, "    restart   %s  backoff=%v..%v reset_after=%v\n",
		s.Restart, s.BackoffInitial.Std(), s.BackoffMax.Std(), s.BackoffResetAfter.Std())
	fmt.Fprintf(w, "    stop      signal=%s timeout=%v\n", s.StopSignal, s.StopTimeout.Std())
	if s.Ready != nil {
		fmt.Fprintf(w, "    ready     cmd=%q interval=%v timeout=%v on_timeout=%s\n",
			s.Ready.Command, s.Ready.Interval.Std(), s.Ready.Timeout.Std(), s.Ready.OnTimeout)
	}
	if s.ReloadSignal != "" {
		fmt.Fprintf(w, "    reload    signal=%s\n", s.ReloadSignal)
	} else if len(s.ReloadCommand) > 0 {
		fmt.Fprintf(w, "    reload    cmd=%q\n", s.ReloadCommand)
	}
	if len(s.ReloadOnChange) > 0 {
		fmt.Fprintf(w, "    reload_on_change %v\n", s.ReloadOnChange)
	}
	n := s.Replicas.N
	if n < 1 {
		n = 1
	}
	if s.Replicas.Auto {
		fmt.Fprintf(w, "    replicas  auto (currently %d, min=%d max=%d)\n",
			n, s.ReplicasMin, s.ReplicasMax)
	} else if n > 1 {
		fmt.Fprintf(w, "    replicas  %d\n", n)
	}
	// Expanded per-replica log paths. {index} expansion happens here
	// so the operator sees what they would have seen in /var/log/.
	if s.Log.Stdout != "" && s.Log.Stdout != "inherit" {
		if n > 1 || s.Replicas.Auto {
			paths := make([]string, 0, n)
			for i := 0; i < n; i++ {
				paths = append(paths, config.ReplicaLogPath(s.Log.Stdout, i, n))
			}
			fmt.Fprintf(w, "    log.stdout %s\n", strings.Join(dedupStrings(paths), ", "))
		} else {
			fmt.Fprintf(w, "    log.stdout %s\n", s.Log.Stdout)
		}
	}
	if s.Log.Stderr != "" && s.Log.Stderr != "inherit" {
		if n > 1 || s.Replicas.Auto {
			paths := make([]string, 0, n)
			for i := 0; i < n; i++ {
				paths = append(paths, config.ReplicaLogPath(s.Log.Stderr, i, n))
			}
			fmt.Fprintf(w, "    log.stderr %s\n", strings.Join(dedupStrings(paths), ", "))
		} else {
			fmt.Fprintf(w, "    log.stderr %s\n", s.Log.Stderr)
		}
	}
	if len(s.Env) > 0 {
		fmt.Fprintln(w, "    env")
		printSortedMap(w, "      ", s.Env)
	}
	if !s.IsReloadable() {
		fmt.Fprintln(w, "    reloadable false")
	}
	fmt.Fprintln(w)
}

func printSortedMap(w io.Writer, prefix string, m map[string]string) {
	if len(m) == 0 {
		fmt.Fprintf(w, "%s(empty)\n", prefix)
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s=%s\n", prefix, k, m[k])
	}
}

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// listEntrypointScripts walks the entrypoint.d directory and returns
// the basenames in the order they would run, applying the same
// hidden/disabled/non-executable filtering the entrypoint package
// itself uses at boot. Pure read; never executes anything.
func listEntrypointScripts(configDir string) []string {
	entries, err := os.ReadDir(configDir + "/entrypoint.d")
	if err != nil {
		return nil
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
			continue
		}
		marker := ""
		if info.Mode()&0o111 == 0 {
			marker = " (NOT EXECUTABLE, would be skipped at runtime)"
		}
		out = append(out, n+marker)
	}
	sort.Strings(out)
	return out
}
