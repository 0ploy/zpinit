//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// wrapSysProcAttr is the --init wrap child's: its own process group
// (foreground on the terminal when there is one), and SIGKILL if zpinit
// dies, matching supervised services.
func wrapSysProcAttr(foreground bool) *syscall.SysProcAttr {
	a := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if foreground {
		a.Foreground = true
		a.Ctty = 0
	}
	return a
}

func isTerminal(fd int) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}
