//go:build !linux

package main

import "syscall"

func wrapSysProcAttr(bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func isTerminal(int) bool { return false }
