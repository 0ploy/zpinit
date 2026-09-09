package resources

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupLayout locates the directories that hold THIS process's cgroup
// limit files.
//
// Reading them is not as simple as looking at the cgroupfs mount root.
// The mount root is the container's own cgroup only when the runtime
// put it there — which is Docker's default under `--cgroupns=private`,
// but NOT under `--cgroupns=host` (the default on cgroup v1 hosts), nor
// when someone bind-mounts the host's /sys/fs/cgroup into the container
// (cAdvisor-style monitoring sidecars), nor under some Kubernetes/CRI
// and LXC layouts. In those cases the mount root is the HOST's root
// cgroup, which has no cpu.max/memory.max at all, so limit detection
// silently falls back to the host's /proc view and reports the whole
// machine's budget to a container limited to a fraction of it.
type cgroupLayout struct {
	// v2 is the unified-hierarchy directory for this process, or ""
	// when the host is not on cgroup v2.
	v2 string
	// v1 maps a controller name ("cpu", "memory", "cpuset") to this
	// process's directory for that controller. Empty on a v2 host.
	v1 map[string]string
	// resolved is true when the layout came from /proc/self/cgroup plus
	// /proc/self/mountinfo, i.e. we know these are OUR cgroup's files.
	// False means we fell back to assuming the mount root, which is
	// right for the common case and wrong in exactly the situations
	// described above — so a false here means "treat the numbers as
	// unverified", not "there is no limit".
	resolved bool
}

func (l cgroupLayout) v1dir(controller string) string { return l.v1[controller] }

// resolveCgroupLayout works out where this process's cgroup files live.
//
// Deliberately not cached: the layout cannot change for a running
// process, but caching it would freeze the ZPINIT_CGROUP_ROOT override
// across tests that set it per-case. The cost is two small /proc reads
// on an operation that now runs a handful of times an hour.
func resolveCgroupLayout() cgroupLayout {
	// Test override: treat the given directory as both the unified root
	// and the v1 controller parent, which is the legacy layout every
	// existing fixture builds.
	if root := os.Getenv("ZPINIT_CGROUP_ROOT"); root != "" {
		return legacyLayout(root, true)
	}
	if l, ok := resolveFromProc("/proc/self/cgroup", "/proc/self/mountinfo"); ok {
		return l
	}
	return legacyLayout("/sys/fs/cgroup", false)
}

// legacyLayout is the pre-resolution assumption: the cgroupfs mount
// root IS our cgroup, with v1 controllers in subdirectories.
func legacyLayout(root string, resolved bool) cgroupLayout {
	return cgroupLayout{
		v2: root,
		v1: map[string]string{
			"cpu":    filepath.Join(root, "cpu"),
			"memory": filepath.Join(root, "memory"),
			"cpuset": filepath.Join(root, "cpuset"),
		},
		resolved: resolved,
	}
}

func resolveFromProc(cgroupFile, mountinfoFile string) (cgroupLayout, bool) {
	cf, err := os.Open(cgroupFile)
	if err != nil {
		return cgroupLayout{}, false
	}
	defer cf.Close()
	v2Path, v1Paths := parseSelfCgroup(cf)

	mf, err := os.Open(mountinfoFile)
	if err != nil {
		return cgroupLayout{}, false
	}
	defer mf.Close()
	v2Mount, v1Mounts := parseCgroupMounts(mf)

	out := cgroupLayout{v1: map[string]string{}}
	if v2Mount.mountPoint != "" && v2Path != "" {
		if dir, ok := v2Mount.dirFor(v2Path); ok {
			out.v2 = dir
			out.resolved = true
		}
	}
	for controller, m := range v1Mounts {
		p, ok := v1Paths[controller]
		if !ok {
			continue
		}
		if dir, ok := m.dirFor(p); ok {
			out.v1[controller] = dir
			out.resolved = true
		}
	}
	if !out.resolved {
		return cgroupLayout{}, false
	}
	return out, true
}

// mountEntry is the part of a /proc/<pid>/mountinfo line we need: which
// slice of the filesystem is mounted (root) and where (mountPoint).
type mountEntry struct {
	root       string
	mountPoint string
}

// dirFor maps a cgroup path from /proc/self/cgroup onto a real
// directory under this mount. The mount exposes the subtree `root` at
// `mountPoint`, so a cgroup path only resolves if it lies within root,
// and the answer is mountPoint + the remainder.
//
//	root=/  mount=/sys/fs/cgroup  path=/            -> /sys/fs/cgroup
//	root=/  mount=/sys/fs/cgroup  path=/docker/abc  -> /sys/fs/cgroup/docker/abc
//	root=/docker/abc  mount=/sys/fs/cgroup  path=/docker/abc -> /sys/fs/cgroup
func (m mountEntry) dirFor(cgroupPath string) (string, bool) {
	if m.mountPoint == "" {
		return "", false
	}
	root := m.root
	if root == "" {
		root = "/"
	}
	if root == "/" {
		return filepath.Join(m.mountPoint, cgroupPath), true
	}
	if cgroupPath == root {
		return m.mountPoint, true
	}
	if rest, ok := strings.CutPrefix(cgroupPath, root+"/"); ok {
		return filepath.Join(m.mountPoint, rest), true
	}
	// Our cgroup is outside the mounted subtree: the files are simply
	// not reachable from in here.
	return "", false
}

// parseSelfCgroup reads /proc/<pid>/cgroup.
//
//	v2:  "0::/docker/abc"
//	v1:  "4:cpu,cpuacct:/docker/abc"
//
// Returns the unified path (empty if the host is v1-only) and a
// controller -> path map for v1. A v1 line lists several controllers
// sharing one hierarchy, so each gets its own entry.
func parseSelfCgroup(r io.Reader) (v2Path string, v1Paths map[string]string) {
	v1Paths = map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// hierarchy-ID : controller-list : cgroup-path. The path may
		// itself contain ':', so split into exactly three.
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		controllers, path := parts[1], parts[2]
		if controllers == "" {
			// The unified hierarchy always has an empty controller
			// field and hierarchy ID 0.
			v2Path = path
			continue
		}
		for _, c := range strings.Split(controllers, ",") {
			// Named v1 hierarchies appear as "name=systemd"; they carry
			// no limits, so skip them.
			if strings.HasPrefix(c, "name=") {
				continue
			}
			v1Paths[c] = path
		}
	}
	return v2Path, v1Paths
}

// parseCgroupMounts reads /proc/<pid>/mountinfo and returns the cgroup2
// mount plus the v1 mounts keyed by controller.
//
// Line shape (fields 7..n are optional and terminated by a lone "-"):
//
//	476 475 0:41 / /sys/fs/cgroup ro,relatime - cgroup2 cgroup rw
//	 |   |   |   |       |                       |            |
//	 id par maj root  mountPoint               fstype     superOptions
//
// For v1 the controller names appear in the super options
// ("rw,cpu,cpuacct"), which is how a mount is matched to a controller.
func parseCgroupMounts(r io.Reader) (v2 mountEntry, v1 map[string]mountEntry) {
	v1 = map[string]mountEntry{}
	sc := bufio.NewScanner(r)
	// mountinfo lines are short, but a pathological mount table can
	// exceed bufio's 64KiB default line cap; give it room rather than
	// silently truncating the scan.
	sc.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+len(" - "):])
		if len(left) < 5 || len(right) < 3 {
			continue
		}
		entry := mountEntry{
			root:       unescapeOctal(left[3]),
			mountPoint: unescapeOctal(left[4]),
		}
		switch right[0] {
		case "cgroup2":
			// Several cgroup2 mounts can exist (a hybrid host mounts one
			// under /sys/fs/cgroup/unified); the first is the one the
			// container is meant to use.
			if v2.mountPoint == "" {
				v2 = entry
			}
		case "cgroup":
			for _, opt := range strings.Split(right[2], ",") {
				switch opt {
				case "cpu", "cpuacct", "memory", "cpuset":
					if _, seen := v1[opt]; !seen {
						v1[opt] = entry
					}
				}
			}
		}
	}
	return v2, v1
}

// unescapeOctal decodes the \040-style escapes the kernel writes into
// mountinfo path fields for space, tab, newline and backslash.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 4
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
