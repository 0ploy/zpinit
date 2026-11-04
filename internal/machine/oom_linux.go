//go:build linux

package machine

import (
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
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
// CPU. Only SIGKILL deaths pay it.
const oomRetry = 20 * time.Millisecond

// Killed reports whether the kernel log records an OOM kill of pid
// since the given time (from Now, taken before the spawn).
func (k KernelOOM) Killed(pid int, since time.Duration) bool {
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(oomRetry)
		}
		recs, err := readKmsg()
		if err != nil {
			return false
		}
		if oomKilledIn(recs, pid, since) {
			return true
		}
	}
	return false
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
