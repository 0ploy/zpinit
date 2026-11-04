// Package execproto is the wire protocol of zpinit's machine-mode exec
// listener: the microVM's `docker exec`, served by zpinit (PID 1 of the
// guest) over AF_VSOCK and spoken by the host (a VM manager's exec,
// test harnesses). It is public so every client imports the one codec
// instead of re-implementing it; zpinit's server lives in
// internal/vsockexec. Standard library only.
//
// Compatibility: this package is an interface between separately
// released projects. Frame type values, payload shapes and limits only
// change additively; an incompatible change needs a new Version.
//
// Wire protocol v1, one exec per connection, connections concurrent:
//
//  1. The host sends the request: a 4-byte big-endian length, then that
//     many bytes of JSON (Request).
//  2. zpinit replies with one frame, FrameStarted or FrameError.
//  3. Both sides then exchange frames: 1-byte type, 4-byte big-endian
//     payload length, payload. The session ends with FrameExit (or
//     FrameError) and zpinit closes the connection.
//
// A connection that drops before the exit SIGKILLs the exec's session
// (every process group the exec started): an exec that outlives its
// connection is a process nobody accounts for. The full contract is docs/machine-mode.md.
package execproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version a Request must carry.
const Version = 1

// FrameType is the first byte of every frame.
type FrameType byte

const (
	// FrameStarted (to host): the exec is running. Payload: {"pid": n}.
	FrameStarted FrameType = 1
	// FrameStdin (to guest): bytes for the exec's stdin.
	FrameStdin FrameType = 2
	// FrameStdinEOF (to guest): no more stdin. Empty payload. Closes
	// the stdin pipe; with a tty, sends the terminal's EOF character.
	FrameStdinEOF FrameType = 3
	// FrameResize (to guest): {"rows": r, "cols": c}. tty only.
	FrameResize FrameType = 4
	// FrameSignal (to guest): a signal name ("SIGINT"), delivered to the
	// exec's process group.
	FrameSignal FrameType = 5
	// FrameStdout (to host): bytes. With a tty, the merged terminal
	// stream.
	FrameStdout FrameType = 6
	// FrameStderr (to host): bytes. Without a tty only.
	FrameStderr FrameType = 7
	// FrameExit (to host): {"status": n} or {"signal": "SIGKILL"}; the
	// connection closes after it.
	FrameExit FrameType = 8
	// FrameError (to host): a message; the connection closes after it.
	FrameError FrameType = 9
)

func (t FrameType) String() string {
	switch t {
	case FrameStarted:
		return "started"
	case FrameStdin:
		return "stdin"
	case FrameStdinEOF:
		return "stdin_eof"
	case FrameResize:
		return "resize"
	case FrameSignal:
		return "signal"
	case FrameStdout:
		return "stdout"
	case FrameStderr:
		return "stderr"
	case FrameExit:
		return "exit"
	case FrameError:
		return "error"
	}
	return fmt.Sprintf("frame(%d)", byte(t))
}

// MaxPayload caps a frame payload and the request. Larger is a protocol
// error, so a hostile or broken peer cannot make PID 1 allocate without
// bound.
const MaxPayload = 1 << 20

// Request is the JSON the host sends first.
type Request struct {
	V          int      `json:"v"`
	Argv       []string `json:"argv"`
	User       string   `json:"user,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	Env        []string `json:"env,omitempty"`
	TTY        bool     `json:"tty"`
	Rows       uint16   `json:"rows,omitempty"`
	Cols       uint16   `json:"cols,omitempty"`
}

// Started is the FrameStarted payload.
type Started struct {
	PID int `json:"pid"`
}

// Resize is the FrameResize payload.
type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Exit is the FrameExit payload: exactly one of Status or Signal.
type Exit struct {
	Status *int   `json:"status,omitempty"`
	Signal string `json:"signal,omitempty"`
}

// ReadRequest reads and strictly decodes the length-prefixed request.
func ReadRequest(r io.Reader) (*Request, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("read request length: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxPayload {
		return nil, fmt.Errorf("request length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read request: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	// More() alone misses a stray closing bracket ("{...}}"); only EOF
	// after the object is clean.
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the request")
	}
	if req.V != Version {
		return nil, fmt.Errorf("unsupported protocol version %d (want %d)", req.V, Version)
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return nil, errors.New("argv is required")
	}
	return &req, nil
}

// WriteRequest is the client side of ReadRequest.
func WriteRequest(w io.Writer, req *Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(b) > MaxPayload {
		return fmt.Errorf("request too large (%d bytes)", len(b))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	_, err = w.Write(append(hdr[:], b...))
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("frame payload %d exceeds %d", n, MaxPayload)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return FrameType(hdr[0]), payload, nil
}

// WriteFrame writes one frame in a single Write, so concurrent writers
// serialised by a mutex never interleave a header with another payload.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("frame payload %d exceeds %d", len(payload), MaxPayload)
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = byte(t)
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// WriteJSONFrame marshals v as the payload.
func WriteJSONFrame(w io.Writer, t FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, b)
}
