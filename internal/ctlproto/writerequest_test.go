package ctlproto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The wire is space-delimited and newline-terminated, so a field
// carrying whitespace cannot round-trip. Rejecting beats sanitising
// here: turning a space into something else would silently produce a
// DIFFERENT valid command rather than an error.
func TestWriteRequest_RejectsUnrepresentableFields(t *testing.T) {
	tests := []struct {
		name string
		req  *Request
	}{
		{"space in an argument", &Request{Verb: "status", Args: []string{"a b"}}},
		{"newline in an argument", &Request{Verb: "status", Args: []string{"a\nstop all"}}},
		{"carriage return in an argument", &Request{Verb: "status", Args: []string{"a\rb"}}},
		{"tab in an argument", &Request{Verb: "status", Args: []string{"a\tb"}}},
		{"space in the verb", &Request{Verb: "sta tus"}},
		{"newline in the verb", &Request{Verb: "status\nstop"}},
		{"control character", &Request{Verb: "status", Args: []string{"a\x01b"}}},
		{"DEL", &Request{Verb: "status", Args: []string{"a\x7fb"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := NewConn(&buf).WriteRequest(tc.req)
			if err == nil {
				t.Fatalf("WriteRequest accepted %q; wrote %q", tc.req, buf.String())
			}
			if !errors.Is(err, errFieldNotRepresentable) {
				t.Fatalf("err = %v, want errFieldNotRepresentable", err)
			}
			if buf.Len() != 0 {
				t.Errorf("rejected request still wrote %q to the wire", buf.String())
			}
		})
	}
}

// The injection this closes: without the check, an embedded newline
// would let one WriteRequest emit two lines. The daemon reads exactly
// one, so the tail is silently dropped rather than executed — but the
// request that IS executed is not the one the caller asked for.
func TestWriteRequest_NoSecondLineCanBeSmuggled(t *testing.T) {
	var buf bytes.Buffer
	_ = NewConn(&buf).WriteRequest(&Request{Verb: "status", Args: []string{"x\nstop all"}})
	if strings.Count(buf.String(), "\n") > 1 {
		t.Fatalf("more than one line reached the wire: %q", buf.String())
	}
}

func TestWriteRequest_RoundTripsLegalRequests(t *testing.T) {
	for _, req := range []*Request{
		{Verb: "status"},
		{Verb: "status", Args: []string{"--verbose", "--json"}},
		{Verb: "restart", Args: []string{"php-fpm/3"}},
		{Verb: "signal", Args: []string{"nginx", "HUP"}},
		{Verb: "tail", Args: []string{"--follow", "worker/0"}},
	} {
		var buf bytes.Buffer
		if err := NewConn(&buf).WriteRequest(req); err != nil {
			t.Fatalf("WriteRequest(%v): %v", req, err)
		}
		got, err := NewConn(&buf).ReadRequest()
		if err != nil {
			t.Fatalf("ReadRequest: %v", err)
		}
		if got.Verb != req.Verb || len(got.Args) != len(req.Args) {
			t.Fatalf("round trip: got %+v, want %+v", got, req)
		}
		for i := range req.Args {
			if got.Args[i] != req.Args[i] {
				t.Fatalf("arg %d: got %q want %q", i, got.Args[i], req.Args[i])
			}
		}
	}
}
