//go:build linux

package machine

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DisableCAD makes the kernel deliver Ctrl+Alt+Del to PID 1 as SIGINT
// instead of resetting the machine on the spot. Machine mode's first
// act. Never called in a container: there it would be EPERM (no
// CAP_SYS_BOOT) or EINVAL (reboot_pid_ns rejects CAD_OFF), but the gate
// is the point, not the failure.
func DisableCAD() error {
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_CAD_OFF)
}

// killGrace bounds the wait for processes to die after kill(-1,
// SIGKILL). A process in uninterruptible sleep cannot be reaped until
// its syscall returns; the machine still has to end.
const killGrace = 5 * time.Second

// tcsbrk is TCSBRK; with a non-zero argument it is tcdrain(3).
const tcsbrk = 0x5409

// End ends the machine and never returns. The caller has already run
// the graceful teardown (services stopped, or the wrap child ended).
// What is left:
//
//  1. kill(-1, SIGKILL) for everything still alive (orphans, exec
//     sessions, daemonised strays), reaping until ECHILD;
//  2. sync;
//  3. unmount the other block-backed filesystems in reverse mount order
//     (falling back to a read-only remount, then a lazy unmount), then
//     remount / read-only;
//  4. sync again;
//  5. the reset line on the console;
//  6. reboot(RESTART). Firecracker runs guests with reboot=k, and a
//     guest reset ends the VMM. If reboot fails there is nothing left
//     but to sleep: PID 1 must never return.
func End(log *slog.Logger, s *Settings, o Outcome) {
	if log == nil {
		log = slog.Default()
	}
	log.Info("ending the machine", "cause", o.Cause, "status", o.Code)

	killAll()
	syscall.Sync()
	settleMounts(log, "/proc/self/mounts")
	syscall.Sync()

	if s != nil && s.Console != "" {
		if err := writeResetLine(s.Console, ResetLine(o)); err != nil {
			log.Error("reset line not written", "console", s.Console, "err", err)
		}
	}

	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		log.Error("reboot failed; parking PID 1", "err", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

// killAll SIGKILLs every process but this one and reaps them. The
// supervise-mode reaper goroutine may still be running and win some of
// the wait4 calls; that is fine, either way the child is gone.
func killAll() {
	_ = syscall.Kill(-1, syscall.SIGKILL)
	deadline := time.Now().Add(killGrace)
	for time.Now().Before(deadline) {
		pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.ECHILD):
			return
		case errors.Is(err, syscall.EINTR), err == nil && pid > 0:
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type mountInfo struct {
	source, target, fstype string
}

// parseMounts reads /proc/self/mounts. Fields are octal-escaped
// (a space is \040).
func parseMounts(path string) []mountInfo {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []mountInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		out = append(out, mountInfo{
			source: unescapeMount(fields[0]),
			target: unescapeMount(fields[1]),
			fstype: fields[2],
		})
	}
	return out
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// blockBacked reports whether a mount holds data worth flushing: a
// filesystem on a device. Pseudo filesystems (proc, sysfs, devtmpfs,
// tmpfs, cgroup2, devpts) are left alone.
func blockBacked(m mountInfo) bool {
	return strings.HasPrefix(m.source, "/dev/")
}

// settlePlan returns, in order, the mounts to settle: / last-but-not-
// unmounted, everything else in reverse mount order.
func settlePlan(mounts []mountInfo) (others []mountInfo, root *mountInfo) {
	for i := len(mounts) - 1; i >= 0; i-- {
		m := mounts[i]
		if !blockBacked(m) {
			continue
		}
		if m.target == "/" {
			if root == nil {
				mm := m
				root = &mm
			}
			continue
		}
		others = append(others, m)
	}
	return others, root
}

func settleMounts(log *slog.Logger, path string) {
	others, root := settlePlan(parseMounts(path))
	for _, m := range others {
		err := syscall.Unmount(m.target, 0)
		if err == nil {
			continue
		}
		if rerr := remountRO(m.target); rerr == nil {
			continue
		}
		if derr := syscall.Unmount(m.target, syscall.MNT_DETACH); derr != nil {
			log.Warn("could not unmount", "target", m.target, "err", err)
		}
	}
	// The root disk cannot be unmounted while PID 1 runs on it. zpinit
	// itself runs from a tmpfs and holds nothing open on the disk, so
	// with every other process gone a read-only remount succeeds. If it
	// does not, sync plus the filesystem journal still leave it
	// consistent.
	if root != nil {
		if err := remountRO("/"); err != nil {
			log.Warn("could not remount / read-only", "err", err)
		}
	}
}

func remountRO(target string) error {
	return syscall.Mount("", target, "", syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
}

// writeResetLine opens the console directly, so the line is written even
// when zpinit's own stdio goes nowhere, and waits for the UART to drain:
// reboot(2) would otherwise cut the line off mid-transmit. O_NOCTTY,
// because PID 1 is a session leader and would otherwise acquire the
// console as its controlling terminal.
//
// Only a character device is written. The path comes from machine.json
// (or is salvaged from one that failed validation) and the write lands
// at offset 0, so a console that names a disk (/dev/vda) would
// otherwise overwrite its first sector. O_NONBLOCK keeps the open from
// hanging on a FIFO before the type check can refuse it; os.NewFile
// then parks a write that would block in the runtime poller.
func writeResetLine(console, line string) error {
	fd, err := syscall.Open(console, syscall.O_WRONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: console, Err: err}
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return &os.PathError{Op: "fstat", Path: console, Err: err}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFCHR {
		syscall.Close(fd)
		return fmt.Errorf("%s: not a character device", console)
	}
	f := os.NewFile(uintptr(fd), console)
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	if rc, err := f.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) {
			_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, tcsbrk, 1)
		})
	}
	return nil
}
