package resources

import (
	"strings"
	"testing"
)

// Captured verbatim from `docker run --cgroupns=private` on a cgroup v2
// host (Docker 29, kernel 7.0.12): the container's own cgroup IS the
// mount root, so the pre-resolution assumption happened to be correct.
const (
	selfCgroupV2Private = "0::/\n"
	mountinfoV2         = "476 475 0:41 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n"
)

// Same host, same image, only --cgroupns=host: the mount root is now
// the HOST's root cgroup and our limits live several levels down. This
// is the layout that made Detect report the whole machine's budget.
const selfCgroupV2Host = "0::/docker/ddaba9ca81eb7ef691f94ba3eec9a520b82bc9759451903358181822e911d7e9\n"

func TestParseSelfCgroup(t *testing.T) {
	t.Run("v2 at the root", func(t *testing.T) {
		v2, v1 := parseSelfCgroup(strings.NewReader(selfCgroupV2Private))
		if v2 != "/" {
			t.Errorf("v2 path = %q, want %q", v2, "/")
		}
		if len(v1) != 0 {
			t.Errorf("v1 paths = %v, want none", v1)
		}
	})

	t.Run("v2 nested under cgroupns=host", func(t *testing.T) {
		v2, _ := parseSelfCgroup(strings.NewReader(selfCgroupV2Host))
		want := "/docker/ddaba9ca81eb7ef691f94ba3eec9a520b82bc9759451903358181822e911d7e9"
		if v2 != want {
			t.Errorf("v2 path = %q, want %q", v2, want)
		}
	})

	t.Run("v1 hierarchies", func(t *testing.T) {
		in := "" +
			"11:cpuset:/docker/abc\n" +
			"5:cpu,cpuacct:/docker/abc\n" +
			"4:memory:/docker/abc\n" +
			"1:name=systemd:/docker/abc\n"
		v2, v1 := parseSelfCgroup(strings.NewReader(in))
		if v2 != "" {
			t.Errorf("v2 path = %q, want empty on a v1-only host", v2)
		}
		for _, c := range []string{"cpuset", "cpu", "cpuacct", "memory"} {
			if v1[c] != "/docker/abc" {
				t.Errorf("v1[%q] = %q, want /docker/abc", c, v1[c])
			}
		}
		// Named hierarchies carry no limits and must not be mistaken
		// for a controller.
		if _, ok := v1["name=systemd"]; ok {
			t.Error("named hierarchy leaked into the controller map")
		}
	})

	t.Run("a cgroup path containing a colon survives", func(t *testing.T) {
		v2, _ := parseSelfCgroup(strings.NewReader("0::/weird:name/leaf\n"))
		if v2 != "/weird:name/leaf" {
			t.Errorf("v2 path = %q; SplitN(...,3) is required to keep the path intact", v2)
		}
	})
}

func TestParseCgroupMounts(t *testing.T) {
	v2, v1 := parseCgroupMounts(strings.NewReader(mountinfoV2))
	if v2.mountPoint != "/sys/fs/cgroup" || v2.root != "/" {
		t.Fatalf("v2 mount = %+v, want root=/ mountPoint=/sys/fs/cgroup", v2)
	}
	if len(v1) != 0 {
		t.Errorf("v1 mounts = %v, want none on a v2 host", v1)
	}

	t.Run("v1 controllers come from the super options", func(t *testing.T) {
		in := "" +
			"30 25 0:26 /docker/abc /sys/fs/cgroup/cpu ro,relatime - cgroup cgroup rw,cpu,cpuacct\n" +
			"31 25 0:27 /docker/abc /sys/fs/cgroup/memory ro,relatime - cgroup cgroup rw,memory\n"
		_, v1 := parseCgroupMounts(strings.NewReader(in))
		if got := v1["cpu"]; got.mountPoint != "/sys/fs/cgroup/cpu" || got.root != "/docker/abc" {
			t.Errorf("v1[cpu] = %+v", got)
		}
		if got := v1["memory"]; got.mountPoint != "/sys/fs/cgroup/memory" {
			t.Errorf("v1[memory] = %+v", got)
		}
	})

	t.Run("optional fields before the separator are skipped", func(t *testing.T) {
		in := "476 475 0:41 / /sys/fs/cgroup rw shared:12 master:3 - cgroup2 cgroup rw\n"
		v2, _ := parseCgroupMounts(strings.NewReader(in))
		if v2.mountPoint != "/sys/fs/cgroup" {
			t.Errorf("mountPoint = %q; the variable optional-field section must be handled", v2.mountPoint)
		}
	})

	t.Run("octal escapes in paths are decoded", func(t *testing.T) {
		in := `476 475 0:41 / /sys/fs/cg\040roup rw - cgroup2 cgroup rw` + "\n"
		v2, _ := parseCgroupMounts(strings.NewReader(in))
		if v2.mountPoint != "/sys/fs/cg roup" {
			t.Errorf("mountPoint = %q, want the \\040 decoded to a space", v2.mountPoint)
		}
	})
}

// dirFor is where the actual bug lived: the old code assumed the answer
// was always the mount point.
func TestMountEntryDirFor(t *testing.T) {
	tests := []struct {
		name       string
		root       string
		mountPoint string
		cgroupPath string
		want       string
		wantOK     bool
	}{
		{"cgroupns=private: our cgroup is the mount root",
			"/", "/sys/fs/cgroup", "/", "/sys/fs/cgroup", true},
		{"cgroupns=host: our cgroup is nested below the mount root",
			"/", "/sys/fs/cgroup", "/docker/abc", "/sys/fs/cgroup/docker/abc", true},
		{"v1 bind mount: the mounted subtree IS our cgroup",
			"/docker/abc", "/sys/fs/cgroup/cpu", "/docker/abc", "/sys/fs/cgroup/cpu", true},
		{"v1 bind mount with a child cgroup below it",
			"/docker/abc", "/sys/fs/cgroup/cpu", "/docker/abc/init", "/sys/fs/cgroup/cpu/init", true},
		{"our cgroup lies outside the mounted subtree",
			"/docker/abc", "/sys/fs/cgroup/cpu", "/docker/other", "", false},
		{"a sibling whose name merely shares a prefix is not inside",
			"/docker/abc", "/sys/fs/cgroup/cpu", "/docker/abcdef", "", false},
		{"no mount point at all",
			"/", "", "/", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mountEntry{root: tc.root, mountPoint: tc.mountPoint}.dirFor(tc.cgroupPath)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("dirFor(%q) = (%q, %v), want (%q, %v)", tc.cgroupPath, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// End to end over the two captured layouts: the same mountinfo must
// yield different directories depending on our cgroup path, which is
// exactly what the old mount-root assumption could not express.
func TestResolveFromProc_CapturedLayouts(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := dir + "/" + name
		writeFile(t, p, content)
		return p
	}
	mi := write("mountinfo", mountinfoV2)

	t.Run("private", func(t *testing.T) {
		l, ok := resolveFromProc(write("cg-private", selfCgroupV2Private), mi)
		if !ok || !l.resolved {
			t.Fatalf("resolve failed: %+v ok=%v", l, ok)
		}
		if l.v2 != "/sys/fs/cgroup" {
			t.Errorf("v2 dir = %q, want /sys/fs/cgroup", l.v2)
		}
	})

	t.Run("host", func(t *testing.T) {
		l, ok := resolveFromProc(write("cg-host", selfCgroupV2Host), mi)
		if !ok || !l.resolved {
			t.Fatalf("resolve failed: %+v ok=%v", l, ok)
		}
		want := "/sys/fs/cgroup/docker/ddaba9ca81eb7ef691f94ba3eec9a520b82bc9759451903358181822e911d7e9"
		if l.v2 != want {
			t.Errorf("v2 dir = %q, want %q", l.v2, want)
		}
	})

	t.Run("missing files fall back rather than erroring", func(t *testing.T) {
		if _, ok := resolveFromProc(dir+"/nope", dir+"/nope"); ok {
			t.Error("resolve reported success with no /proc files")
		}
	})
}

// The ZPINIT_CGROUP_ROOT override must keep meaning exactly what every
// existing fixture assumes: root is the unified dir and the v1
// controllers are subdirectories of it.
func TestResolveCgroupLayout_OverrideKeepsLegacyShape(t *testing.T) {
	t.Setenv("ZPINIT_CGROUP_ROOT", "/tmp/fake")
	l := resolveCgroupLayout()
	if l.v2 != "/tmp/fake" {
		t.Errorf("v2 = %q", l.v2)
	}
	if l.v1dir("cpu") != "/tmp/fake/cpu" || l.v1dir("memory") != "/tmp/fake/memory" {
		t.Errorf("v1 dirs = %v", l.v1)
	}
	if !l.resolved {
		t.Error("an explicit override is authoritative and must count as resolved")
	}
}
