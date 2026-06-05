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
	defer f.Close()

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
	if err := emitAvailable(reader, pc, conn); err != nil {
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
			f.Close()
			f, _, err = openRegularNoFollow(path)
			if err != nil {
				_ = pc.WriteBodyLine(fmt.Sprintf("zpinit: reopen: %v", err))
				return
			}
			reader = bufio.NewReader(f)
			prevIno = inodeOf(newSt)
			log.Info("tail --follow: file rotated; reopened", "path", path)
		}
		if err := emitAvailable(reader, pc, conn); err != nil {
			return
		}
	}
}

// emitAvailable drains every complete line currently in the reader,
// writes each as a body line, and returns nil at EOF (more bytes
// may arrive later). Returns an error if the client write fails so
// the streaming loop can exit promptly on disconnect.
func emitAvailable(reader *bufio.Reader, pc *ctlproto.Conn, conn net.Conn) error {
	// Refresh the write deadline on every drain so a long-running
	// follow doesn't time out on the kernel's socket buffer side.
	_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if werr := pc.WriteBodyLine(trimmed); werr != nil {
				return werr
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
