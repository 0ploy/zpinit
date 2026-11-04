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

// controlsTerminal reports whether fd is a terminal AND zpinit's
// controlling terminal. Only then can the child put its group in the
// foreground: TIOCSPGRP on any other tty fails with ENOTTY, the fork
// fails, and the CMD never runs. That is the case on a machine booted
// with init=zpinit, where the kernel hands PID 1 /dev/console as stdio
// but never as a controlling terminal. TIOCGPGRP fails the same way
// for a tty that is not ours, so it answers both questions at once.
func controlsTerminal(fd int) bool {
	var pgrp int32
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&pgrp)))
	return e == 0
}
