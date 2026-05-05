package main

import (
	"flag"
	"fmt"
	"os"
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

	fmt.Fprintln(os.Stderr, "zpinit: not yet implemented (Phase 1+)")
	os.Exit(2)
}
