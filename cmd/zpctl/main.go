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

	// Local short-circuit: `zpctl help` (and -h / --help) prints local
	// usage without dialing the socket, so it works in containers where
	// zpinit isn't running yet (e.g. interactive debug shells, image
	// inspection). The daemon-side help endpoint mirrors this list, so
	// nothing is lost by answering locally.
	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return
	}

	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zpctl: connect %s: %v\n", socket, err)
		os.Exit(2)
	}
	defer conn.Close()

	streaming := isStreamingCmd(args)
	if !streaming {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	pc := ctlproto.NewConn(conn)
	if err := pc.WriteRequest(&ctlproto.Request{Verb: args[0], Args: args[1:]}); err != nil {
		fmt.Fprintf(os.Stderr, "zpctl: send: %v\n", err)
		os.Exit(2)
	}

	if streaming {
		runStreamingClient(conn, pc)
		return
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

// isStreamingCmd reports whether the verb+args combination expects
// a long-running streaming response. Today only `tail --follow`
// (or its `-f` alias) qualifies; new streaming verbs would be
// added here.
func isStreamingCmd(args []string) bool {
	if args[0] != "tail" {
		return false
	}
	for _, a := range args[1:] {
		if a == "--follow" || a == "-f" {
			return true
		}
	}
	return false
}

// runStreamingClient drives the read side of a streaming response:
// status line first, then body lines printed as they arrive until
// the server writes the terminator or the connection closes.
// Ctrl-C in the shell sends SIGINT to zpctl; the OS tears down the
// socket and the server's write fails on the next body line,
// stopping the follow loop cleanly.
func runStreamingClient(conn net.Conn, pc *ctlproto.Conn) {
	code, msg, err := pc.ReadStatusLine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zpctl: read status: %v\n", err)
		os.Exit(2)
	}
	if code != 0 {
		fmt.Fprintf(os.Stderr, "zpctl: %s\n", msg)
		// Drain any body the server may emit before its terminator
		// so we don't leave bytes on the wire.
		for {
			_, done, rerr := pc.ReadBodyLine()
			if rerr != nil || done {
				break
			}
		}
		os.Exit(code)
	}
	for {
		// Refresh the read deadline per line so a long pause without
		// log output doesn't time the client out, but a wedged
		// connection still eventually fails.
		_ = conn.SetReadDeadline(time.Now().Add(24 * time.Hour))
		line, done, rerr := pc.ReadBodyLine()
		if rerr != nil {
			// io.EOF without a terminator (server closed mid-stream)
			// is reported as an unclean shutdown so CI loops notice.
			fmt.Fprintf(os.Stderr, "zpctl: stream ended: %v\n", rerr)
			os.Exit(2)
		}
		if done {
			return
		}
		fmt.Println(line)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: zpctl [--socket PATH] COMMAND [ARGS...]

Commands match supervisorctl naming where possible.

Common commands:
  status [--verbose] [NAME...]
                          list service states; --verbose adds RSS/CPU/fd/spawns
  start NAME[/N] | all    start service(s)
  stop NAME[/N] | all     stop service(s)
  restart NAME[/N] | all  stop then start
  pid [NAME[/N]]          PID of zpinit or a service replica
  ready [NAME[/N]...]     exit 0 iff selected services are Running and [ready] passed
  tail [--follow|-f] NAME[/N]
                          dump file-logged stdout (last 8KB); --follow streams new lines
  update                  apply config changes (= SIGHUP)
  reload [NAME[/N]...]    in-place reload (reload_signal/_command or full restart);
                          no args is equivalent to update
  reread                  dry-run config diff
  signal NAME[/N] SIG     send arbitrary signal
  shutdown                stop supervisor

NAME refers to a service; for services with replicas > 1, NAME selects
every replica and NAME/N selects replica N (0..replicas-1).

`)
}
