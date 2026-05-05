package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev"

func main() {
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	fmt.Fprintln(os.Stderr, "zpctl: not yet implemented (Phase 8)")
	os.Exit(2)
}
