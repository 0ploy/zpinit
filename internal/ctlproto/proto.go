// Package ctlproto defines the line-based plaintext wire format
// between zpinit's control server and zpctl. One request, one response,
// one connection — no streaming, no multiplexing.
//
// Wire format:
//
//	Request : "<verb> [arg ...]\n"
//	Response: "<code> <msg>\n"           // status line; code 0 == OK
//	          "<body line>\n"  (zero or more)
//	          ".\n"                       // body terminator
//
// Plaintext is deliberate — operators debug with nc/socat. No
// dot-stuffing in v1: no implemented command produces a body line that
// starts with ".". If a future command (e.g. raw log streaming) needs
// arbitrary content, revisit.
package ctlproto

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const Terminator = "."

// MaxLineLen caps the size of any single line read from the wire.
// Without this, a misbehaving (or malicious) local client could keep
// sending bytes with no newline and grow our bufio buffer until OOM.
// 64 KiB is comfortably larger than any legitimate verb+args + status
// line + body line we produce.
const MaxLineLen = 64 * 1024

type Request struct {
	Verb string
	Args []string
}

type Response struct {
	Code int      // 0 == OK; non-zero is a client-visible error
	Msg  string   // status-line message (single line)
	Body []string // additional lines, may be empty
}

// Conn pairs a buffered reader with a buffered writer for the lifetime
// of a single connection. The writer is buffered so a status response
// over many services flushes in one syscall instead of one per body
// line.
type Conn struct {
	r *bufio.Reader
	w *bufio.Writer
}

func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReader(rw), w: bufio.NewWriter(rw)}
}

func (c *Conn) ReadRequest() (*Request, error) {
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil, errors.New("empty request")
	}
	return &Request{Verb: parts[0], Args: parts[1:]}, nil
}

func (c *Conn) WriteRequest(req *Request) error {
	var b strings.Builder
	b.Grow(len(req.Verb) + 1)
	b.WriteString(req.Verb)
	for _, a := range req.Args {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	b.WriteByte('\n')
	if _, err := io.WriteString(c.w, b.String()); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *Conn) ReadResponse() (*Response, error) {
	statusLine, err := c.readLine()
	if err != nil {
		return nil, err
	}
	code, msg, err := parseStatus(statusLine)
	if err != nil {
		return nil, err
	}
	resp := &Response{Code: code, Msg: msg}
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if line == Terminator {
			return resp, nil
		}
		resp.Body = append(resp.Body, line)
	}
}

func (c *Conn) WriteResponse(resp *Response) error {
	if _, err := fmt.Fprintf(c.w, "%d %s\n", resp.Code, sanitizeLine(resp.Msg)); err != nil {
		return err
	}
	for _, b := range resp.Body {
		if _, err := fmt.Fprintln(c.w, sanitizeLine(b)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(c.w, Terminator); err != nil {
		return err
	}
	return c.w.Flush()
}

// sanitizeLine fixes content that would break the line-based wire
// frame: CR/LF would split a single field across multiple lines, and
// a body line consisting solely of "." (Terminator) would end the
// body early at the client. Service log content surfaced by `tail`
// and TOML parse errors surfaced by `update` can each contain
// newlines, so doing this once at the wire layer covers every
// caller. Replacement is intentionally lossy (whitespace) rather
// than escaped — the protocol has no decoder for escapes and we'd
// rather render mangled than risk a malformed frame.
func sanitizeLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") && s != Terminator {
		return s
	}
	if s == Terminator {
		return " " + Terminator
	}
	r := strings.NewReplacer("\r", " ", "\n", " ")
	return r.Replace(s)
}

// errLineTooLong signals that the peer sent more than MaxLineLen bytes
// without a newline. Treated as a hard read failure.
var errLineTooLong = errors.New("line too long")

func (c *Conn) readLine() (string, error) {
	// ReadSlice returns ErrBufferFull instead of growing without bound;
	// we cap to MaxLineLen so a misbehaving local client can't OOM PID 1.
	var buf []byte
	for {
		chunk, err := c.r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			if len(buf)+len(chunk) > MaxLineLen {
				return "", errLineTooLong
			}
			buf = append(buf, chunk...)
			continue
		}
		if err != nil && len(chunk) == 0 && len(buf) == 0 {
			return "", err
		}
		if len(buf)+len(chunk) > MaxLineLen {
			return "", errLineTooLong
		}
		buf = append(buf, chunk...)
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimRight(string(buf), "\r\n"), nil
	}
}

func parseStatus(line string) (int, string, error) {
	sp := strings.SplitN(line, " ", 2)
	code, err := strconv.Atoi(sp[0])
	if err != nil {
		return 0, "", fmt.Errorf("invalid status line: %q", line)
	}
	msg := ""
	if len(sp) > 1 {
		msg = sp[1]
	}
	return code, msg, nil
}
