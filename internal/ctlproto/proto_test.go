package ctlproto

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRoundTrip_Request(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf)
	if err := c.WriteRequest(&Request{Verb: "status", Args: []string{"redis", "nginx"}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadRequest()
	if err != nil {
		t.Fatal(err)
	}
	if got.Verb != "status" {
		t.Errorf("verb = %q", got.Verb)
	}
	if len(got.Args) != 2 || got.Args[0] != "redis" || got.Args[1] != "nginx" {
		t.Errorf("args = %v", got.Args)
	}
}

func TestReadBodyLine_PartialLineAtEOFIsError(t *testing.T) {
	// A peer that dies mid-line must not have the fragment accepted as
	// a complete line: a truncated "." would otherwise read as a clean
	// terminator and a crashed-mid-stream follow would exit 0.
	var w bytes.Buffer
	c := NewConn(struct {
		io.Reader
		io.Writer
	}{strings.NewReader("0 ok\nbody\n."), &w})
	if code, _, err := c.ReadStatusLine(); err != nil || code != 0 {
		t.Fatalf("status: code=%d err=%v", code, err)
	}
	if line, done, err := c.ReadBodyLine(); err != nil || done || line != "body" {
		t.Fatalf("body: line=%q done=%v err=%v", line, done, err)
	}
	// The trailing "." has no newline: EOF mid-line, must error.
	if _, done, err := c.ReadBodyLine(); err == nil {
		t.Fatalf("partial terminator at EOF: done=%v err=nil, want error", done)
	}
}

func TestRoundTrip_Response(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf)
	want := &Response{Code: 0, Msg: "ok", Body: []string{"line1", "line2"}}
	if err := c.WriteResponse(want); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadResponse()
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 || got.Msg != "ok" {
		t.Errorf("status = %d %q", got.Code, got.Msg)
	}
	if len(got.Body) != 2 || got.Body[0] != "line1" || got.Body[1] != "line2" {
		t.Errorf("body = %v", got.Body)
	}
}

func TestEmptyRequest(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("\n")
	c := NewConn(&buf)
	if _, err := c.ReadRequest(); err == nil {
		t.Fatal("expected error on empty request")
	}
}

func TestSanitizeLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ok", "ok"},
		{"line\nwith\nnewlines", "line with newlines"},
		{"crlf\r\nmix", "crlf  mix"},
		{".", " ."},
		{"..", ".."},
	}
	for _, c := range cases {
		if got := sanitizeLine(c.in); got != c.want {
			t.Errorf("sanitizeLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteResponse_RobustToTaintedBody(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf)
	// Body line containing both a newline (would split) and a lone "."
	// (would terminate body early). After sanitization the wire frame
	// must still round-trip cleanly with both body lines preserved.
	want := &Response{
		Code: 0,
		Msg:  "ok\nbroken",
		Body: []string{"normal line", ".", "embedded\nnewline"},
	}
	if err := c.WriteResponse(want); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadResponse()
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if got.Code != 0 {
		t.Errorf("code = %d", got.Code)
	}
	if strings.Contains(got.Msg, "\n") {
		t.Errorf("msg still contains newline: %q", got.Msg)
	}
	if len(got.Body) != 3 {
		t.Fatalf("body lines = %d, want 3 (body framing broke): %v", len(got.Body), got.Body)
	}
	for _, b := range got.Body {
		if strings.Contains(b, "\n") {
			t.Errorf("body line still contains newline: %q", b)
		}
	}
}

func TestErrorResponse(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf)
	if err := c.WriteResponse(&Response{Code: 1, Msg: "unknown service: ghost"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadResponse()
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 {
		t.Errorf("code = %d", got.Code)
	}
	if !strings.Contains(got.Msg, "unknown service") {
		t.Errorf("msg = %q", got.Msg)
	}
	if len(got.Body) != 0 {
		t.Errorf("body = %v", got.Body)
	}
}
