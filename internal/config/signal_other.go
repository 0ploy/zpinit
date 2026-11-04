//go:build !linux

package config

import "syscall"

var osSignals = map[string]syscall.Signal{}
