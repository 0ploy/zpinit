//go:build linux

package machine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSettlePlan(t *testing.T) {
	// A parked-after-switch VM: the disk at /, a data disk, pseudo
	// filesystems, and a mount with an escaped space.
	mounts := `/dev/vdb / ext4 rw,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
devtmpfs /dev devtmpfs rw,nosuid,size=4k 0 0
tmpfs /run tmpfs rw,nosuid,nodev 0 0
tmpfs /run/zpinit/bin tmpfs ro,nosuid,nodev 0 0
cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime 0 0
/dev/vdc /data ext4 rw,relatime 0 0
/dev/vdd /data/with\040space xfs rw,relatime 0 0
`
	p := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(p, []byte(mounts), 0o644); err != nil {
		t.Fatal(err)
	}
	others, root := settlePlan(parseMounts(p))
	if root == nil || root.source != "/dev/vdb" {
		t.Fatalf("root = %+v", root)
	}
	// Reverse mount order, block-backed only, / excluded.
	want := []string{"/data/with space", "/data"}
	if len(others) != len(want) {
		t.Fatalf("others = %+v, want targets %v", others, want)
	}
	for i, m := range others {
		if m.target != want[i] {
			t.Errorf("others[%d] = %q, want %q", i, m.target, want[i])
		}
	}
}

func TestSettlePlanInitramfs(t *testing.T) {
	// Rescue before the switch: / is the initramfs rootfs, nothing to
	// settle.
	p := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(p, []byte("rootfs / rootfs rw 0 0\nproc /proc proc rw 0 0\ntmpfs /run tmpfs rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	others, root := settlePlan(parseMounts(p))
	if root != nil || len(others) != 0 {
		t.Errorf("others=%v root=%v, want nothing", others, root)
	}
}

// The reset line goes only to a character device. machine.json names
// the path, and the write lands at offset 0: a console naming a disk or
// a file must be refused, not overwritten, and a FIFO must not hang the
// open.
func TestWriteResetLineRefusesNonCharDevice(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	const content = "superblock"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	line := ResetLine(Outcome{Cause: CauseExit, Code: 3})
	for _, p := range []string{file, fifo, dir} {
		done := make(chan error, 1)
		go func() { done <- writeResetLine(p, line) }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: written; want refused", p)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: writeResetLine hung", p)
		}
	}
	if got, _ := os.ReadFile(file); string(got) != content {
		t.Errorf("file overwritten: %q", got)
	}
}

// A character device that is not a tty still takes the line (the
// tcdrain ioctl is best effort).
func TestWriteResetLineToCharDevice(t *testing.T) {
	if err := writeResetLine(os.DevNull, ResetLine(Outcome{Cause: CauseExit})); err != nil {
		t.Fatal(err)
	}
}
