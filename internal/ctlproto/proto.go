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

type Request struct {
	Verb string
	Args []string
}

type Response struct {
	Code int      // 0 == OK; non-zero is a client-visible error
	Msg  string   // status-line message (single line)
	Body []string // additional lines, may be empty
}

// Conn pairs a buffered reader with a writer for the lifetime of a
// single connection.
type Conn struct {
	r *bufio.Reader
	w io.Writer
}

func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReader(rw), w: rw}
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
	out := req.Verb
	for _, a := range req.Args {
		out += " " + a
	}
	_, err := io.WriteString(c.w, out+"\n")
	return err
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
	if _, err := fmt.Fprintf(c.w, "%d %s\n", resp.Code, resp.Msg); err != nil {
		return err
	}
	for _, b := range resp.Body {
		if _, err := fmt.Fprintln(c.w, b); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(c.w, Terminator)
	return err
}

func (c *Conn) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
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
