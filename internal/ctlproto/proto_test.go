package ctlproto

import (
	"bytes"
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
