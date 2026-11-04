package execproto

import (
	"bytes"
	"testing"
)

func TestReadRequestStrict(t *testing.T) {
	var b bytes.Buffer
	body := `{"v":1,"argv":["sh"],"tty":false,"shell":true}`
	b.Write([]byte{0, 0, 0, byte(len(body))})
	b.WriteString(body)
	if _, err := ReadRequest(&b); err == nil {
		t.Error("unknown request field accepted")
	}
	for _, body := range []string{`{"v":1,"argv":["sh"],"tty":false}}`, `{"v":1,"argv":["sh"],"tty":false} x`} {
		b.Reset()
		b.Write([]byte{0, 0, 0, byte(len(body))})
		b.WriteString(body)
		if _, err := ReadRequest(&b); err == nil {
			t.Errorf("trailing data accepted: %s", body)
		}
	}
	// The same request without trailing data is fine.
	ok := `{"v":1,"argv":["sh"],"tty":false}`
	b.Reset()
	b.Write([]byte{0, 0, 0, byte(len(ok))})
	b.WriteString(ok)
	if _, err := ReadRequest(&b); err != nil {
		t.Errorf("clean request rejected: %v", err)
	}
	b.Reset()
	b.Write([]byte{0xff, 0xff, 0xff, 0xff})
	if _, err := ReadRequest(&b); err == nil {
		t.Error("oversized request accepted")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrame(&b, FrameStdout, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, FrameStdinEOF, nil); err != nil {
		t.Fatal(err)
	}
	if want := []byte{6, 0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o', 3, 0, 0, 0, 0}; !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("wire = %v, want %v", b.Bytes(), want)
	}
	ft, p, err := ReadFrame(&b)
	if err != nil || ft != FrameStdout || string(p) != "hello" {
		t.Errorf("frame 1 = %v %q %v", ft, p, err)
	}
	ft, p, err = ReadFrame(&b)
	if err != nil || ft != FrameStdinEOF || len(p) != 0 {
		t.Errorf("frame 2 = %v %q %v", ft, p, err)
	}
	if err := WriteFrame(&b, FrameStdin, make([]byte, MaxPayload+1)); err == nil {
		t.Error("oversized frame written")
	}
}

// TestWireValuesPinned pins the values clients depend on. They are an
// interface with separately released hosts: changing one breaks
// every deployed client, so a failure here means "new Version", not
// "update the test".
func TestWireValuesPinned(t *testing.T) {
	want := map[FrameType]byte{
		FrameStarted: 1, FrameStdin: 2, FrameStdinEOF: 3, FrameResize: 4, FrameSignal: 5,
		FrameStdout: 6, FrameStderr: 7, FrameExit: 8, FrameError: 9,
	}
	for ft, b := range want {
		if byte(ft) != b {
			t.Errorf("%s = %d, want %d", ft, byte(ft), b)
		}
	}
	if Version != 1 || MaxPayload != 1<<20 {
		t.Errorf("Version = %d, MaxPayload = %d", Version, MaxPayload)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	var b bytes.Buffer
	in := &Request{V: Version, Argv: []string{"sh", "-l"}, User: "app", WorkingDir: "/srv",
		Env: []string{"A=1"}, TTY: true, Rows: 40, Cols: 120}
	if err := WriteRequest(&b, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadRequest(&b)
	if err != nil {
		t.Fatal(err)
	}
	if out.User != "app" || out.WorkingDir != "/srv" || !out.TTY || out.Rows != 40 || out.Cols != 120 ||
		len(out.Argv) != 2 || len(out.Env) != 1 {
		t.Errorf("round trip = %+v", out)
	}
	for name, req := range map[string]*Request{
		"version": {V: 2, Argv: []string{"x"}},
		"no argv": {V: 1},
	} {
		b.Reset()
		if err := WriteRequest(&b, req); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRequest(&b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
