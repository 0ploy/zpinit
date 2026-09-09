package resources

import "log/slog"

// cgroupNotify turns a write to one of the container's cgroup limit
// files into a wake-up, so the Watcher can sleep instead of poll.
//
// The kernel raises IN_MODIFY on these files through the generic
// vfs_write path, so any userspace writer (dockerd/containerd on
// `docker update`, the kubelet on an in-place pod resize, systemd)
// produces an event. Nothing else does: reads do not, and neither does
// load on the container, so an idle watcher costs zero wakeups.
//
// It is an accelerator, never the source of truth. Two things it cannot
// see keep the Watcher's backstop poll load-bearing:
//
//   - a limit the kernel derives internally rather than by a write to
//     OUR file (a parent cgroup's cpuset propagating into
//     cpuset.cpus.effective), and
//   - a runtime that accepts the watch but never delivers (some sandbox
//     kernels emulate cgroupfs), which is silent and undetectable.
//
// Every method is called only from the Watcher's loop goroutine, so
// implementations need no locking of their own beyond the wake channel.
type cgroupNotify interface {
	// C returns the wake channel. A receive means only "a limit file
	// was written; re-Detect" — never which file or which dimension.
	// The runtime writes cpu.max and memory.max separately and
	// non-atomically, so an event can arrive against a half-applied
	// change; the caller re-reads everything and lets the debounce
	// settle it. Events coalesce (cap-1 channel): a burst is one wake.
	C() <-chan struct{}

	// rearm (re)establishes the watches. Idempotent by design —
	// inotify_add_watch on an already-watched path returns the same
	// descriptor — so the caller can simply call it on every
	// observation instead of tracking watch state. That is what makes
	// the notifier self-healing: a watch dropped because the inode went
	// away (IN_IGNORED after a cgroupfs remount) or never armed because
	// the per-UID watch budget was exhausted (ENOSPC, e.g. a file
	// watcher in the same container) comes back on the next call.
	rearm()

	// close releases the inotify instance and stops the reader.
	close()
}

// newCgroupNotify builds the platform notifier for root, or nil when
// this build has no implementation (non-Linux dev builds). A nil
// notifier is valid everywhere: the Watcher selects on a nil channel,
// which blocks forever, and falls back to its backstop poll.
func newCgroupNotify(log *slog.Logger) cgroupNotify {
	return newCgroupNotifyPlatform(cgroupLimitPaths(resolveCgroupLayout()), log)
}
