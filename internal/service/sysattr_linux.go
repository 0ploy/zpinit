//go:build linux

package service

import "syscall"

// baseSysProcAttr returns a SysProcAttr configured for our spawn model:
//   - Setpgid puts the child in its own process group, so PGID kill reaches
//     forks and double-forks (php-fpm master + workers, etc.).
//   - Pdeathsig SIGKILLs the child if zpinit itself dies; belt-and-braces
//     for the case where the supervisor crashes before reaping.
//
// Pdeathsig caveat: the kernel fires PR_SET_PDEATHSIG when the spawning
// THREAD dies, not the process. That is safe today only because nothing
// in zpinit calls runtime.LockOSThread (a locked goroutine's exit kills
// its thread, which would SIGKILL every child spawned from it). If a
// future change needs LockOSThread, spawning must move off that thread.
func baseSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}
