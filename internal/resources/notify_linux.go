//go:build linux

package resources

import (
	"log/slog"
	"os"
	"syscall"
)

// inotifyNotifier watches the cgroup limit files for IN_MODIFY.
//
// Only IN_MODIFY is requested, deliberately. IN_ACCESS / IN_OPEN /
// IN_CLOSE_NOWRITE would also fire on every Detect(), and since a wake
// triggers a Detect that is a self-sustaining event loop.
type inotifyNotifier struct {
	paths []string
	ch    chan struct{}
	log   *slog.Logger

	// f owns the inotify fd; nil until the first successful rearm and
	// again never after close. fd is the same descriptor, kept raw
	// because inotify_add_watch needs it. Both are touched only from
	// the Watcher's loop goroutine (see cgroupNotify), so no lock; the
	// reader goroutine holds its own *os.File reference instead.
	f  *os.File
	fd int

	// armed is the number of paths watched as of the last rearm. Only
	// used to log the arm/disarm transition once rather than on every
	// observation.
	armed  int
	logged bool
}

func newCgroupNotifyPlatform(paths []string, log *slog.Logger) cgroupNotify {
	return &inotifyNotifier{
		paths: paths,
		// cap 1: a wake means "re-Detect", so a burst (the runtime
		// writes cpu.max and memory.max as separate writes) collapses
		// into one observation. Non-blocking sends keep the reader off
		// the loop goroutine's critical path.
		ch:  make(chan struct{}, 1),
		log: log,
	}
}

func (n *inotifyNotifier) C() <-chan struct{} { return n.ch }

func (n *inotifyNotifier) rearm() {
	if n.f == nil {
		// IN_NONBLOCK so os.NewFile registers the fd with the runtime
		// poller: the reader parks in netpoll rather than pinning an OS
		// thread, and Close unblocks it cleanly.
		fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
		if err != nil {
			// EMFILE/ENFILE/ENOMEM. Retried on the next observation;
			// the backstop poll covers the gap.
			n.log.Debug("cgroup inotify unavailable; falling back to the backstop poll", "err", err)
			return
		}
		n.fd = fd
		n.f = os.NewFile(uintptr(fd), "cgroup-inotify")
		go n.read(n.f)
	}
	armed := 0
	for _, p := range n.paths {
		// ENOENT for the other hierarchy version's paths is the normal
		// case, not an error: cgroupLimitPaths lists v1 and v2 together
		// and lets the kernel say which exist.
		if _, err := syscall.InotifyAddWatch(n.fd, p, syscall.IN_MODIFY); err != nil {
			continue
		}
		armed++
	}
	if armed != n.armed || !n.logged {
		switch {
		case armed == 0:
			n.log.Info("no cgroup limit file could be watched; relying on the backstop poll")
		case n.logged:
			n.log.Info("cgroup limit watches re-established", "files", armed)
		default:
			n.log.Info("watching cgroup limit files for changes", "files", armed)
		}
		n.armed = armed
		n.logged = true
	}
}

// read forwards every inotify event as a single wake. Event contents
// are deliberately not parsed: the wake only means "re-Detect", and the
// Watcher's own comparison decides whether anything actually moved.
func (n *inotifyNotifier) read(f *os.File) {
	buf := make([]byte, 4096)
	for {
		if _, err := f.Read(buf); err != nil {
			// Closed by close(), or an unrecoverable read error. Either
			// way this reader is done; a later rearm builds a new one.
			return
		}
		select {
		case n.ch <- struct{}{}:
		default: // a wake is already pending; nothing to add
		}
	}
}

func (n *inotifyNotifier) close() {
	if n.f == nil {
		return
	}
	// Closing the *os.File unblocks the reader's parked Read.
	_ = n.f.Close()
	n.f = nil
	n.fd = -1
}
