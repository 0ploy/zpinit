package main

import (
	"os"
	"syscall"
	"testing"
)

func has(sigs []os.Signal, s syscall.Signal) bool {
	for _, x := range sigs {
		if x == s {
			return true
		}
	}
	return false
}

// Busybox halt/poweroff signal PID 1 with USR1/USR2. Only the kernel's
// init treats them as a stop; a container keeps forwarding them.
func TestStopSignalsMachineAddsBusyboxHalt(t *testing.T) {
	for _, s := range []syscall.Signal{syscall.SIGUSR1, syscall.SIGUSR2} {
		if has(stopSignals(false), s) {
			t.Errorf("container: %v must not be a stop signal", s)
		}
		if !has(sinkSignals(false), s) {
			t.Errorf("container: %v must be caught by the sink", s)
		}
		if !has(stopSignals(true), s) {
			t.Errorf("machine: %v must be a stop signal", s)
		}
		if has(sinkSignals(true), s) {
			t.Errorf("machine: %v must not also reach the sink (it would be forwarded to the wrap child)", s)
		}
	}
}

// No signal may be both a stop signal and forwarded by the sink.
func TestStopAndSinkDisjoint(t *testing.T) {
	for _, m := range []bool{false, true} {
		for _, s := range stopSignals(m) {
			if has(sinkSignals(m), s.(syscall.Signal)) {
				t.Errorf("machine=%v: %v in both sets", m, s)
			}
		}
	}
}
