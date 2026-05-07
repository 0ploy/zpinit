package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/0ploy/zpinit/internal/ctlproto"
)

var version = "dev"

const defaultSocket = "/run/zpinit.sock"

func main() {
	var (
		showVersion bool
		socket      string
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&socket, "socket", defaultSocket, "control socket `path`")
	flag.Usage = usage
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	// Local short-circuit: `zpctl help` should work even with no daemon.
	// Forwarding to the daemon would fail with a connection error.
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zpctl: connect %s: %v\n", socket, err)
		os.Exit(2)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	pc := ctlproto.NewConn(conn)
	if err := pc.WriteRequest(&ctlproto.Request{Verb: args[0], Args: args[1:]}); err != nil {
		fmt.Fprintf(os.Stderr, "zpctl: send: %v\n", err)
		os.Exit(2)
	}
	resp, err := pc.ReadResponse()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zpctl: read response: %v\n", err)
		os.Exit(2)
	}
	for _, line := range resp.Body {
		fmt.Println(line)
	}
	if resp.Code != 0 {
		fmt.Fprintf(os.Stderr, "zpctl: %s\n", resp.Msg)
		os.Exit(resp.Code)
	}
	// Suppress the trivial "ok" status line; print anything else.
	if resp.Msg != "" && resp.Msg != "ok" {
		fmt.Println(resp.Msg)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: zpctl [--socket PATH] COMMAND [ARGS...]

Commands match supervisorctl naming where possible. Run "zpctl help"
against a running zpinit for the full list.

Common commands:
  status [NAME...]      list service states
  start NAME | all      start service(s)
  stop NAME | all       stop service(s)
  restart NAME | all    stop then start
  pid [NAME]            PID of zpinit or a service
  tail NAME             dump last 8KB of file-logged stdout
  update                apply config changes (= SIGHUP)
  reread                dry-run config diff
  signal NAME SIG       send arbitrary signal
  shutdown              stop supervisor

`)
}
