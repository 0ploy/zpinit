//go:build linux

package vsockexec

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// AF_VSOCK and friends. The standard syscall package does not carry
// them and the only approved dependency is the TOML decoder, so the
// socket is driven with raw syscalls.
const (
	afVsock = 40
	// CIDAny binds every local CID.
	cidAny = 0xFFFFFFFF
	// CIDHost is VMADDR_CID_HOST, the hypervisor side. The only peer
	// accepted.
	CIDHost = 2
)

// rawSockaddrVM is struct sockaddr_vm (16 bytes).
type rawSockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Flags     uint8
	Zero      [3]uint8
}

// Listener is an AF_VSOCK stream listener.
type Listener struct {
	f *os.File
}

// ListenVsock binds port on every local CID.
func ListenVsock(port uint32) (*Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	sa := rawSockaddrVM{Family: afVsock, Port: port, CID: cidAny}
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", port, e)
	}
	if err := syscall.Listen(fd, 64); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}
	// Non-blocking, so os.NewFile registers it with the runtime poller:
	// Accept parks instead of pinning a thread, and Close unblocks it.
	return &Listener{f: os.NewFile(uintptr(fd), "vsock:"+strconv.FormatUint(uint64(port), 10))}, nil
}

// Accept returns the next connection and its peer CID.
func (l *Listener) Accept() (*os.File, uint32, error) {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return nil, 0, err
	}
	var (
		nfd    int
		peer   rawSockaddrVM
		accErr error
	)
	err = rc.Read(func(fd uintptr) bool {
		alen := uint32(unsafe.Sizeof(peer))
		r, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, fd, uintptr(unsafe.Pointer(&peer)), uintptr(unsafe.Pointer(&alen)),
			syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0, 0)
		if e == syscall.EAGAIN {
			return false
		}
		if e != 0 {
			accErr = e
			return true
		}
		nfd = int(r)
		return true
	})
	if err != nil {
		return nil, 0, err
	}
	if accErr != nil {
		return nil, 0, accErr
	}
	return os.NewFile(uintptr(nfd), "vsock-conn"), peer.CID, nil
}

// Close stops the listener; a blocked Accept returns an error.
func (l *Listener) Close() error { return l.f.Close() }

// openPTY allocates a pseudo-terminal pair on the devpts the bringup
// init mounted. The master is pollable (reads park in the runtime poller);
// the slave is handed to the child as its stdio and controlling tty.
func openPTY(rows, cols uint16) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (*os.File, *os.File, error) {
		master.Close()
		return nil, nil, e
	}
	var n uint32
	if err := ioctl(master, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&n))); err != nil {
		return fail(fmt.Errorf("unlockpt: %w", err))
	}
	if err := ioctl(master, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		return fail(fmt.Errorf("ptsname: %w", err))
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	if rows > 0 && cols > 0 {
		_ = setWinsize(master, rows, cols)
	}
	return master, slave, nil
}

type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

func setWinsize(f *os.File, rows, cols uint16) error {
	ws := winsize{Row: rows, Col: cols}
	return ioctl(f, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

func ioctl(f *os.File, req, arg uintptr) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var e syscall.Errno
	if cerr := rc.Control(func(fd uintptr) {
		_, _, e = syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	}); cerr != nil {
		return cerr
	}
	if e != 0 {
		return e
	}
	return nil
}

// isEIO reports the error a pty master read returns once every slave
// fd is closed: the terminal's end of stream.
func isEIO(err error) bool {
	return errors.Is(err, syscall.EIO)
}

// eofChar is what stdin_eof writes to a tty: the default VEOF (^D).
const eofChar = 0x04
