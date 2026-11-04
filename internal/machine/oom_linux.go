//go:build linux

package machine

import (
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/0ploy/zpinit/internal/reaper"
)

// KernelOOM is the reaper's OOM detector in machine mode; see oom.go.
type KernelOOM struct{}

// Now is CLOCK_MONOTONIC: time since boot, the reference the kernel
// log's timestamps are compared against.
func (KernelOOM) Now() time.Duration {
	var ts syscall.Timespec
	const clockMonotonic = 1
	if _, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockMonotonic, uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		return 0
	}
	return time.Duration(ts.Nano())
}

// oomRetry: the OOM killer logs its victim right after sending
// SIGKILL, so the record is normally there before the reap. One short
// retry covers the case where the victim is reaped first on another
// CPU. Only a batch with an unmatched SIGKILL death pays it, and the
// reaper runs this off its reap loop.
const oomRetry = 20 * time.Millisecond

// Killed reports, per candidate, whether the kernel log records an OOM
// kill of its pid since its Since (from Now, taken before the spawn).
// The log is read once for the whole batch, and once more only if a
// candidate is still unmatched.
func (k KernelOOM) Killed(cands []reaper.OOMCandidate) []bool {
	out := make([]bool, len(cands))
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(oomRetry)
		}
		recs, err := readKmsg()
		if err != nil {
			return out
		}
		if attributeOOM(oomVictims(recs), cands, out) {
			return out
		}
	}
	return out
}

// readKmsg reads every record currently in the kernel log buffer.
// /dev/kmsg returns one record per read; EAGAIN (O_NONBLOCK) marks the
// end, and EPIPE means the reader fell behind a wrapped buffer, which
// is skipped.
func readKmsg() ([][]byte, error) {
	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]byte
	buf := make([]byte, 8192)
	for {
		n, err := syscall.Read(int(f.Fd()), buf)
		switch {
		case errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.EINTR):
			continue
		case err != nil || n <= 0:
			return out, nil
		}
		out = append(out, append([]byte(nil), buf[:n]...))
	}
}
