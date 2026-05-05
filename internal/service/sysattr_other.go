//go:build !linux

package service

import "syscall"

// baseSysProcAttr for non-Linux builds (development on macOS only). No
// Pdeathsig — that's a Linux-specific PR_SET_PDEATHSIG. Production
// always runs on Linux.
func baseSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid: true,
	}
}
