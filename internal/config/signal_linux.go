//go:build linux

package config

import "syscall"

// osSignals are the Linux-only names (absent from syscall on macOS).
var osSignals = map[string]syscall.Signal{
	"STKFLT": syscall.SIGSTKFLT,
	"PWR":    syscall.SIGPWR,
}
