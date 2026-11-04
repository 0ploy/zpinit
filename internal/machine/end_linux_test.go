//go:build linux

package machine

import (
	"os"
	"path/filepath"
	"testing"
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

func TestWriteResetLineToFile(t *testing.T) {
	// writeResetLine must work on a non-tty too (the tcdrain ioctl is
	// best effort).
	p := filepath.Join(t.TempDir(), "console")
	if err := os.WriteFile(p, []byte("partial workload output"), 0o644); err != nil {
		t.Fatal(err)
	}
	line := ResetLine(Outcome{Cause: CauseExit, Code: 3})
	if err := writeResetLine(p, line); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	// O_WRONLY without O_APPEND overwrites from offset 0, which is what
	// a tty does not care about; the content check is on the line.
	if string(got[:len(line)]) != line {
		t.Errorf("console = %q", got)
	}
}
