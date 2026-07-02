package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/ctlproto"
)

// cmdTailFollow streams new lines as they're appended to a
// service's stdout log file until the client disconnects or the
// supervisor shuts down. Polls with os.Stat + ReadAt rather than
// inotify so it works on every container filesystem (tmpfs,
// overlayfs, NFS — inotify is famously unreliable on the second
// and third).
//
// Detects log rotation by inode change (logrotate's default mode
// renames the old file and creates a new one). When the inode
// moves, the next poll reopens the new file from offset 0 so the
// operator's view follows the rotation instead of getting wedged
// on a file that no app writes to anymore.
//
// Wire shape: writes the status line "0 ok" immediately, then
// streams one body line per log line (after sanitization). The
// terminator is written by handleStream when this function
// returns; the client's read loop treats the terminator (or a
// network error) as the end of the stream.
func (s *ControlServer) cmdTailFollow(ctx context.Context, conn net.Conn, pc *ctlproto.Conn, args []string) {
	// Args layout: ["--follow", "name"] or ["name", "--follow"], in
	// either order. Strip the flag (and any -f alias) before name
	// resolution.
	_, args = extractFlag(args, "--follow")
	_, args = extractFlag(args, "-f")
	if len(args) != 1 {
		_ = pc.WriteStatusLine(1, "usage: tail --follow NAME[/N]")
		return
	}
	name := args[0]
	rs, err := resolveTarget(s.orch.snapshotRunners(), name)
	if err != nil {
		code := ctlproto.CodeFailed
		if errors.Is(err, errUnknownService) {
			code = ctlproto.CodeUnknownService
		}
		_ = pc.WriteStatusLine(code, err.Error())
		return
	}
	if len(rs) > 1 {
		_ = pc.WriteStatusLine(1, fmt.Sprintf("%s has %d replicas; specify which one: tail --follow %s/N", name, len(rs), name))
		return
	}
	cfg := rs[0].Cfg()
	if cfg.Log.Stdout == "" || cfg.Log.Stdout == "inherit" {
		_ = pc.WriteStatusLine(1, fmt.Sprintf("%s logs to stdout (no file to tail)", rs[0].DisplayName()))
		return
	}
	if err := pc.WriteStatusLine(0, "ok"); err != nil {
		return
	}
	streamFile(ctx, conn, pc, cfg.Log.Stdout, s.log)
}

// streamFile is the actual follow loop, factored out so future
// callers (e.g. tail --follow on stderr) can reuse it. Initial
// dump is the last 8KB to match one-shot tail; then poll every
// 200ms for size growth, reopening on inode change. Exits when
// ctx fires or a write to the client fails.
func streamFile(ctx context.Context, conn net.Conn, pc *ctlproto.Conn, path string, log *slog.Logger) {
	const initialTail = int64(8192)
	const pollInterval = 200 * time.Millisecond

	f, st, err := openRegularNoFollow(path)
	if err != nil {
		_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: %v", err))
		return
	}
	// Late-bound close: the rotation branch below reassigns f, and a
	// plain `defer f.Close()` would pin the pre-rotation handle,
	// leaking the reopened file's FD on every return after a rotation.
	defer func() { f.Close() }()

	// Emit the last initialTail bytes as the snapshot, just like
	// one-shot `tail`. Pin the offset to the start of the first
	// complete line so half-line snippets don't appear mid-stream.
	offset := st.Size() - initialTail
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: seek: %v", err))
		return
	}
	reader := bufio.NewReader(f)
	if offset > 0 {
		// Drop the first (likely partial) line.
		if _, err := reader.ReadString('\n'); err != nil && err != io.EOF {
			_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: read: %v", err))
			return
		}
	}
	// carry accumulates a partial trailing line across drains so a
	// writer caught mid-line never has its line delivered as two body
	// frames. Bounded by maxFollowChunk (emitAvailable flushes full
	// chunks of oversized lines), so an app that never writes a
	// newline can't grow PID 1's memory.
	var carry []byte
	if err := emitAvailable(reader, pc, conn, &carry); err != nil {
		return
	}

	prevIno := inodeOf(st)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Detect rotation via inode change: logrotate renames the old
		// file out and creates a new one at the same path. When that
		// happens, reopen and reset the reader. Without this, the
		// follow loop would stay parked on the renamed (now dead)
		// inode and never see the new logs.
		newSt, statErr := os.Stat(path)
		if statErr == nil && inodeOf(newSt) != prevIno {
			// The old file's partial line will never complete; emit
			// the prefix rather than dropping it.
			if err := flushCarry(pc, &carry); err != nil {
				return
			}
			f.Close()
			f, _, err = openRegularNoFollow(path)
			if err != nil {
				_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: reopen: %v", err))
				return
			}
			reader = bufio.NewReader(f)
			prevIno = inodeOf(newSt)
			log.Info("tail --follow: file rotated; reopened", "path", path)
		} else if statErr == nil {
			// logrotate's copytruncate mode keeps the inode and rewinds
			// the file, so the inode check above never fires. Detect it
			// by the file shrinking below what we have already consumed
			// and start over from the top; without this the follow loop
			// parks at the stale offset and goes silent forever.
			if pos, serr := f.Seek(0, io.SeekCurrent); serr == nil {
				consumed := pos - int64(reader.Buffered())
				if newSt.Size() < consumed {
					if err := flushCarry(pc, &carry); err != nil {
						return
					}
					if _, serr := f.Seek(0, io.SeekStart); serr != nil {
						_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: seek: %v", serr))
						return
					}
					reader.Reset(f)
					log.Info("tail --follow: file truncated; restarting from top", "path", path)
				}
			}
		}
		if err := emitAvailable(reader, pc, conn, &carry); err != nil {
			return
		}
	}
}

// maxFollowChunk caps the carry buffer for a single log line in the
// follow stream. A line longer than this is flushed in chunks of this
// size: the client rejects wire lines over ctlproto.MaxLineLen (64KiB)
// as a protocol failure, so an unchunked multi-hundred-KiB JSON log
// line would kill the follow with a misleading "unreachable" exit.
// 32KiB leaves ample headroom below the wire cap.
const maxFollowChunk = 32 * 1024

// emitAvailable drains the reader, writes every COMPLETE line as one
// body line, and returns nil at EOF (more bytes may arrive later). A
// partial trailing line stays in carry until its newline shows up in
// a later drain, so a writer caught mid-line never gets its line
// delivered as two frames. Returns an error if the client write fails
// so the streaming loop can exit promptly on disconnect.
func emitAvailable(reader *bufio.Reader, pc *ctlproto.Conn, conn net.Conn, carry *[]byte) error {
	// Refresh the write deadline on every drain so a long-running
	// follow doesn't time out on the kernel's socket buffer side.
	_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	for {
		chunk, err := reader.ReadString('\n')
		if len(chunk) > 0 {
			*carry = append(*carry, chunk...)
			if chunk[len(chunk)-1] == '\n' {
				line := strings.TrimRight(string(*carry), "\r\n")
				*carry = (*carry)[:0]
				if werr := emitChunked(pc, line); werr != nil {
					return werr
				}
			} else if len(*carry) >= maxFollowChunk {
				// Oversized line with no newline in sight: flush what
				// we have so memory stays bounded. The line arrives
				// split across frames; that beats killing the stream
				// over one log line.
				if werr := emitChunked(pc, string(*carry)); werr != nil {
					return werr
				}
				*carry = (*carry)[:0]
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// Read error other than EOF: surface and stop.
			_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: read: %v", err))
			return err
		}
	}
}

// emitChunked writes s as one body line, or as several
// maxFollowChunk-sized body lines when s exceeds the cap. The client
// treats any wire line over ctlproto.MaxLineLen as a protocol failure
// and reports the daemon unreachable, so a single oversized JSON log
// line must never reach the socket unsplit.
func emitChunked(pc *ctlproto.Conn, s string) error {
	for len(s) > maxFollowChunk {
		if err := pc.WriteBodyLine(s[:maxFollowChunk]); err != nil {
			return err
		}
		s = s[maxFollowChunk:]
	}
	return pc.WriteBodyLine(s)
}

// flushCarry emits any partial line held in carry. Called before the
// reader switches files (rotation) or rewinds (truncation): the
// partial line's remainder is gone, so emitting the prefix beats
// silently dropping it.
func flushCarry(pc *ctlproto.Conn, carry *[]byte) error {
	if len(*carry) == 0 {
		return nil
	}
	line := strings.TrimRight(string(*carry), "\r\n")
	*carry = (*carry)[:0]
	if line == "" {
		return nil
	}
	return emitChunked(pc, line)
}

// inodeOf extracts the inode from a FileInfo via the underlying
// syscall.Stat_t. Linux-specific in spirit; on macOS the same
// field exists so this works for dev as well. Returns 0 if the
// info doesn't expose the syscall struct (no platform we ship to
// today hits that).
func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func (s *ControlServer) cmdTail(args []string) *ctlproto.Response {
	if len(args) != 1 {
		return errResp("usage: tail NAME[/N]")
	}
	name := args[0]
	rs, err := resolveTarget(s.orch.snapshotRunners(), name)
	if err != nil {
		return errRespFor(err)
	}
	if len(rs) > 1 {
		return errResp(fmt.Sprintf("%s has %d replicas; specify which one: tail %s/N", name, len(rs), name))
	}
	r := rs[0]
	cfg := r.Cfg()
	if cfg.Log.Stdout == "" || cfg.Log.Stdout == "inherit" {
		return errResp(fmt.Sprintf("%s logs to stdout (no file to tail)", r.DisplayName()))
	}
	body, err := readLastBytes(cfg.Log.Stdout, 8192)
	if err != nil {
		return errRespFor(err)
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	return okBody("ok", lines)
}

// openRegularNoFollow opens path read-only with O_NOFOLLOW and verifies
// it is a regular file, returning the open file and its FileInfo. It is
// the single home for the log-file hardening documented in
// docs/security.md: O_NOFOLLOW rejects a symlink at the leaf (so a
// service config pointing log.stdout at a symlink can't trick `zpctl
// tail` into reading the link target), and the IsRegular check rejects
// device files, FIFOs, and directories that would otherwise hang or
// dump nonsense. Shared by the one-shot read and the follow loop
// (including its post-rotation reopen) so the guarantee can't drift.
func openRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("not a regular file: %s", path)
	}
	return f, st, nil
}

func readLastBytes(path string, n int64) (string, error) {
	f, st, err := openRegularNoFollow(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	offset := st.Size() - n
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return "", err
	}
	buf := make([]byte, st.Size()-offset)
	if _, err := io.ReadFull(f, buf); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	// When the window starts mid-file, the first chunk is almost
	// certainly the tail of a longer line whose head is past the
	// window. Drop it so operators see whole log lines only. When
	// offset == 0 we have the whole file and trim nothing.
	if offset > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return string(buf), nil
}
