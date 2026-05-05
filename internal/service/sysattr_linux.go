//go:build linux

package service

import "syscall"

// baseSysProcAttr returns a SysProcAttr configured for our spawn model:
//   - Setpgid puts the child in its own process group, so PGID kill reaches
//     forks and double-forks (php-fpm master + workers, etc.).
//   - Pdeathsig SIGKILLs the child if zpinit itself dies; belt-and-braces
//     for the case where the supervisor crashes before reaping.
func baseSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}
