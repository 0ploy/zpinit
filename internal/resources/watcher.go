package resources

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DimCPU and DimMemory name the two resource dimensions a Watcher
// reports as changed. Used in Change.Dimensions and in service
// reload_on_change validation.
const (
	DimCPU    = "cpu"
	DimMemory = "memory"
)

// Change is a committed delta emitted by Watcher to subscribers.
// Dimensions is the subset of {DimCPU, DimMemory} whose exposed
// integer/uint64 value moved relative to the last committed
// Snapshot — services key their reload_on_change list against this
// list.
type Change struct {
	Snapshot   Snapshot
	Dimensions []string
}

// Watcher periodically re-runs Detect and emits a Change to
// subscribers when an exposed value (ZPINIT_CPU_COUNT or
// ZPINIT_MEMORY_BYTES) moves and stays moved past the configured
// debounce. Sub-integer wobble in cgroup quota that doesn't change
// the rounded-down CPU count is invisible: that's the "change is
// only a real move of exposed values" rule.
//
// The poll cadence is 1 s by default. Each tick runs a full Detect,
// which re-reads /proc/cpuinfo as well as the cgroup files, so the
// loop is not free on a host running many containers. Callers
// therefore start it only when something consumes a Change (see
// Config.NeedsResourceWatch and Start); inotify on the cgroup limit
// files is the next step for the containers that do need it.
type Watcher struct {
	reserveCPU float64
	reserveMem uint64
	upAfter    time.Duration
	downAfter  time.Duration
	pollEvery  time.Duration

	log *slog.Logger

	// notifier is the platform inotify hook (nil off Linux, or when a
	// test disables it). Field rather than a constructor arg so tests
	// in this package can swap it, mirroring the Orchestrator's
	// spawner/prober hooks: the watcher tests run inside a
	// testing/synctest bubble, and a goroutine parked on real inotify
	// I/O is not "durably blocked", so the bubble's virtual clock would
	// never advance with a live notifier.
	notifier cgroupNotify

	mu      sync.Mutex
	current Snapshot
	subs    []chan Change
	started bool
}

// defaultBackstopInterval is how often the watcher re-detects on its
// own. It is a BACKSTOP, not the primary trigger: inotify on the cgroup
// limit files catches every runtime-driven quota change within
// milliseconds, and this timer exists only for the two things inotify
// cannot see (a kernel-derived limit change, and a sandbox runtime that
// accepts the watch but never delivers). Long on purpose — a host
// running hundreds of containers pays this wakeup once per container
// per interval, and the value it guards changes about never. Operators
// on a runtime with no working inotify shorten it via
// `[resources] poll_interval`.
const defaultBackstopInterval = 15 * time.Minute

// observeCooldown is the minimum gap between two inotify-driven
// observations. Wakes are demand-driven, so a process rewriting a
// cgroup limit file in a loop would otherwise spin the watcher through
// a full Detect per write. Normally unreachable (cgroupfs is mounted
// read-only, so only the host runtime can write it), but cgroup
// delegation — nested containers, systemd-in-container — hands that
// write access to processes inside the container.
//
// A wake inside the window is DEFERRED to the end of it, never
// dropped: it may be the only notice of a real change. Any number of
// wakes in one window collapse into a single later observation, which
// is free because an observation re-reads everything anyway. The
// backstop tick is not gated (its own interval already bounds it) but
// does reset the window.
const observeCooldown = 100 * time.Millisecond

// NewWatcher returns a Watcher with the configured reservations and
// debounce intervals. Zero durations fall back to 5 s for scale-up
// and 30 s for scale-down. Caller must invoke Start to begin
// polling; until then Current returns the zero Snapshot and
// Subscribe channels receive nothing.
func NewWatcher(reserveCPU float64, reserveMem uint64, upAfter, downAfter time.Duration, log *slog.Logger) *Watcher {
	if upAfter <= 0 {
		upAfter = 5 * time.Second
	}
	if downAfter <= 0 {
		downAfter = 30 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{
		reserveCPU: reserveCPU,
		reserveMem: reserveMem,
		upAfter:    upAfter,
		downAfter:  downAfter,
		pollEvery:  defaultBackstopInterval,
		log:        log,
		notifier:   newCgroupNotify(log),
	}
}

// SetPollInterval overrides the backstop re-detect interval (default
// defaultBackstopInterval). Operators reach it through
// `[resources] poll_interval`, which only matters on a runtime where
// inotify on cgroupfs does not deliver; tests use it to keep simulated
// runtime short. Must be called before Start.
func (w *Watcher) SetPollInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	w.pollEvery = d
}

// Current returns the most recently committed Snapshot. Safe to
// call before Start (returns the zero Snapshot) and concurrently
// with the polling goroutine.
func (w *Watcher) Current() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current
}

// Subscribe returns a buffered (cap 1) channel that receives a
// Change every time a debounced commit happens, and a cleanup
// function the caller must invoke when done. A slow subscriber
// drops events: we keep the producer non-blocking so one wedged
// consumer can't pin the watcher goroutine. Channels are not
// closed on Start exit; ctx cancellation stops the producer and
// the channels stay drainable.
//
// The cleanup function removes the subscription from w.subs.
// Without it, every test that constructs a Watcher and subscribes
// leaks an entry, and any production caller that re-subscribes per
// reload would grow w.subs unbounded. Mirrors the Runner.Observe
// pattern in internal/supervisor.
func (w *Watcher) Subscribe() (<-chan Change, func()) {
	ch := make(chan Change, 1)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	cleanup := func() {
		w.mu.Lock()
		for i, c := range w.subs {
			if c == ch {
				// copy+nil+truncate, not append-splice; otherwise the
				// removed entry stays referenced from the backing
				// array's tail and prevents GC.
				copy(w.subs[i:], w.subs[i+1:])
				w.subs[len(w.subs)-1] = nil
				w.subs = w.subs[:len(w.subs)-1]
				break
			}
		}
		w.mu.Unlock()
	}
	return ch, cleanup
}

// Start primes the current Snapshot from a synchronous Detect and
// launches the polling goroutine. Idempotent: a second Start on the
// same Watcher is a no-op.
//
// Returns true only for the call that actually started polling. The
// watcher is started lazily (see Config.NeedsResourceWatch): main.go
// skips it at boot when no service consumes resource deltas and calls
// Start again from the config-commit hook if a reload introduces one.
// Reporting the transition here lets that caller log it without
// tracking a flag of its own, which would need its own lock: the hook
// runs on the control-socket goroutine, not the supervise loop.
func (w *Watcher) Start(ctx context.Context) bool {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return false
	}
	w.current = Detect().WithReserves(w.reserveCPU, w.reserveMem)
	w.started = true
	w.mu.Unlock()
	go w.loop(ctx)
	return true
}

func (w *Watcher) loop(ctx context.Context) {
	// inotify is the primary trigger; the ticker only backstops what it
	// cannot see. A nil notifier yields a nil channel, whose select arm
	// blocks forever, so the backstop silently carries the whole job.
	var notifyCh <-chan struct{}
	if w.notifier != nil {
		notifyCh = w.notifier.C()
		defer w.notifier.close()
		// Arm before the first select so a quota write landing between
		// Start's priming Detect and the loop is not missed.
		w.notifier.rearm()
	}

	poll := time.NewTicker(w.pollEvery)
	defer poll.Stop()

	var (
		pending      *Snapshot
		debounceCh   <-chan time.Time
		debounceTime *time.Timer
	)
	// Cooldown state for inotify wakes. coolTimer, when set, means "at
	// least one wake arrived inside the window and an observation is
	// owed when it expires".
	var (
		lastObserve time.Time
		coolCh      <-chan time.Time
		coolTimer   *time.Timer
	)
	defer func() {
		if coolTimer != nil {
			coolTimer.Stop()
		}
	}()
	cancelDebounce := func() {
		pending = nil
		if debounceTime != nil {
			debounceTime.Stop()
		}
		debounceTime = nil
		debounceCh = nil
	}

	// observe re-detects and (re)arms the debounce timer. Shared
	// verbatim by the inotify wake and the backstop tick: an inotify
	// event carries no usable payload (the runtime writes cpu.max and
	// memory.max as separate, non-atomic writes, so an event can arrive
	// against a half-applied change), so both paths do the same thing —
	// read everything, compare, let the debounce settle it.
	observe := func() {
		// Re-arm on every observation instead of tracking watch state:
		// inotify_add_watch is idempotent, so this is a no-op when
		// healthy and an instant self-heal after an IN_IGNORED (the
		// inode went away under a cgroupfs remount, which silently
		// drops the watch) or a transient ENOSPC from another process
		// in the container exhausting the per-UID watch budget.
		if w.notifier != nil {
			w.notifier.rearm()
		}
		snap := Detect().WithReserves(w.reserveCPU, w.reserveMem)
		w.mu.Lock()
		current := w.current
		w.mu.Unlock()

		if exposedEqual(snap, current) {
			// Either nothing changed, or a brief excursion has
			// returned to baseline before we acted. Either way,
			// no pending commit.
			cancelDebounce()
			return
		}
		if pending != nil && exposedEqual(snap, *pending) {
			// Same target as the one we are already waiting on;
			// keep the existing timer.
			return
		}
		// New (or revised) target. Pick the slower delay if any
		// dimension is moving down — scale-down should be
		// patient, scale-up can be eager. Mixed directions get
		// downAfter for safety.
		delay := w.upAfter
		if isAnyScaleDown(snap, current) {
			delay = w.downAfter
		}
		s := snap
		pending = &s
		if debounceTime != nil {
			debounceTime.Stop()
		}
		debounceTime = time.NewTimer(delay)
		debounceCh = debounceTime.C
	}

	// observeNow runs an observation and opens a fresh cooldown window.
	observeNow := func() {
		lastObserve = time.Now()
		observe()
	}

	for {
		select {
		case <-ctx.Done():
			cancelDebounce()
			return

		case <-notifyCh:
			// Rate-limit the wake path. Deferred, not dropped: a wake
			// can be the only notice that a limit moved, so swallowing
			// one would leave detection stale until the backstop.
			if wait := observeCooldown - time.Since(lastObserve); wait > 0 {
				if coolTimer == nil {
					coolTimer = time.NewTimer(wait)
					coolCh = coolTimer.C
				}
				continue
			}
			observeNow()

		case <-coolCh:
			coolTimer = nil
			coolCh = nil
			observeNow()

		case <-poll.C:
			observeNow()

		case <-debounceCh:
			if pending == nil {
				continue
			}
			// Re-detect at commit time. The pending state may have
			// drifted between the trigger and now, in which case we
			// abandon this commit and let the next poll decide.
			snap := Detect().WithReserves(w.reserveCPU, w.reserveMem)
			pendingSnap := *pending
			cancelDebounce()
			if !exposedEqual(snap, pendingSnap) {
				continue
			}
			w.commit(snap)
		}
	}
}

func (w *Watcher) commit(snap Snapshot) {
	w.mu.Lock()
	dims := changedDimensions(w.current, snap)
	w.current = snap
	subs := append([]chan Change(nil), w.subs...)
	w.mu.Unlock()
	if len(dims) == 0 {
		// Re-entry guard: changedDimensions should always be
		// non-empty here because the debounce path only fires when
		// snap differs from current. Skip the emit anyway rather
		// than push an empty Change.
		return
	}
	change := Change{Snapshot: snap, Dimensions: dims}
	w.log.Info("resources changed",
		"cpu_count", snap.CPUCount,
		"cpu_quota", snap.EnvVars()[EnvCPUQuota],
		"memory_bytes", snap.MemoryBytes,
		"dimensions", dims,
	)
	for _, ch := range subs {
		select {
		case ch <- change:
		default:
			// Subscriber not draining; drop. Resource-change events
			// are state-deltas, not commands; a missed delta is
			// recoverable from Current() on the next subscriber wake.
		}
	}
}

// exposedEqual compares only the integer/uint64 values that zpinit
// exposes to children. CPUQuota wobble that doesn't change the
// floor is intentionally ignored.
func exposedEqual(a, b Snapshot) bool {
	return a.CPUCount == b.CPUCount && a.MemoryBytes == b.MemoryBytes
}

// isAnyScaleDown reports whether any exposed dimension is moving
// downward from current to next.
func isAnyScaleDown(next, current Snapshot) bool {
	if next.CPUCount < current.CPUCount {
		return true
	}
	if next.MemoryBytes < current.MemoryBytes {
		return true
	}
	return false
}

func changedDimensions(prev, next Snapshot) []string {
	var out []string
	if prev.CPUCount != next.CPUCount {
		out = append(out, DimCPU)
	}
	if prev.MemoryBytes != next.MemoryBytes {
		out = append(out, DimMemory)
	}
	return out
}
