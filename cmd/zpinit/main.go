package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/0ploy/zpinit/internal/reaper"
)

var version = "dev"

func main() {
	var (
		showVersion bool
		checkConfig string
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&checkConfig, "check-config", "", "validate configuration in `dir` and exit")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	if checkConfig != "" {
		fmt.Fprintln(os.Stderr, "zpinit: --check-config is not yet implemented (Phase 2)")
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if pid := os.Getpid(); pid != 1 {
		log.Warn("zpinit is not running as PID 1; orphan reaping is unreliable outside containers", "pid", pid)
	}

	os.Exit(run(log))
}

func run(log *slog.Logger) int {
	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	r := reaper.New(log)
	// Drain any pre-existing zombies before entering the loop.
	r.Reap()

	log.Info("zpinit started", "version", version, "pid", os.Getpid())

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGCHLD:
			r.Reap()
		case syscall.SIGTERM, syscall.SIGINT:
			log.Info("shutdown signal received", "signal", sig.String())
			// Final drain — supervisors that exit before reaping leak orphans.
			r.Reap()
			return 0
		case syscall.SIGHUP:
			log.Info("SIGHUP received; reload not yet implemented (Phase 7)")
		}
	}
}
