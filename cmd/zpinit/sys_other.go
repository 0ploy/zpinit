//go:build !linux

package main

import "syscall"

func wrapSysProcAttr(bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func controlsTerminal(int) bool { return false }
