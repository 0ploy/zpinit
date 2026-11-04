//go:build linux

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

// openTestPTY returns a pty pair; the slave is opened O_NOCTTY, so it
// is a terminal but nobody's controlling terminal (the shape of
// /dev/console for the kernel's init).
func openTestPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	t.Cleanup(func() { master.Close() })
	var n uint32
	for _, req := range []uintptr{syscall.TIOCSPTLCK, syscall.TIOCGPTN} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), req, uintptr(unsafe.Pointer(&n))); e != 0 {
			t.Skip("pty setup:", e)
		}
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("open pty slave:", err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// A tty on stdin that is not zpinit's controlling terminal must not get
// a foreground child: TIOCSPGRP fails there and the CMD never starts.
func TestForegroundOnlyOnControllingTerminal(t *testing.T) {
	_, slave := openTestPTY(t)
	if controlsTerminal(int(slave.Fd())) {
		t.Fatal("controlsTerminal: a pty opened O_NOCTTY is not our controlling terminal")
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if controlsTerminal(int(devnull.Fd())) {
		t.Fatal("controlsTerminal(/dev/null) = true")
	}

	// What the old isTerminal predicate led to: Foreground on a tty
	// that is not ours fails the start.
	cmd := exec.Command("true")
	cmd.Stdin = slave
	cmd.SysProcAttr = wrapSysProcAttr(true)
	if err := cmd.Run(); err == nil {
		t.Fatal("Foreground on a non-controlling tty started; the guard is untestable here")
	}
	// What runResidentWrap does now on such a stdin.
	cmd = exec.Command("true")
	cmd.Stdin = slave
	cmd.SysProcAttr = wrapSysProcAttr(controlsTerminal(int(slave.Fd())))
	if err := cmd.Run(); err != nil {
		t.Fatalf("start with a non-controlling tty on stdin: %v", err)
	}
}

// With stdin as the controlling terminal (docker run -t, an interactive
// shell), the child still gets the foreground.
func TestForegroundOnControllingTerminal(t *testing.T) {
	if os.Getenv("ZPINIT_TEST_CTTY_HELPER") == "1" {
		if !controlsTerminal(0) {
			os.Exit(2)
		}
		cmd := exec.Command("true")
		cmd.Stdin = os.Stdin
		cmd.SysProcAttr = wrapSysProcAttr(true)
		if err := cmd.Run(); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	_, slave := openTestPTY(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestForegroundOnControllingTerminal$")
	cmd.Env = append(os.Environ(), "ZPINIT_TEST_CTTY_HELPER=1")
	cmd.Stdin = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		switch ee.ExitCode() {
		case 2:
			t.Fatal("controlsTerminal(0) = false on the session's controlling terminal")
		case 3:
			t.Fatal("foreground child failed to start on the controlling terminal")
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}
