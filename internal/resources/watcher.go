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
// The poll cadence is intentionally cheap (1 s default) and reads
// two small cgroupfs files per tick. inotify on cgroupfs is on the
// roadmap; the poll cost is small enough that we ship without it.
type Watcher struct {
	reserveCPU float64
	reserveMem uint64
	upAfter    time.Duration
	downAfter  time.Duration
	pollEvery  time.Duration

	log *slog.Logger

	mu      sync.Mutex
	current Snapshot
	subs    []chan Change
	started bool
}

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
		pollEvery:  1 * time.Second,
		log:        log,
	}
}

// SetPollInterval overrides the default 1 s poll cadence. Intended
// for tests that need short intervals to keep wall-clock runtime
// reasonable; production should leave the default.
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
// Change every time a debounced commit happens. A slow subscriber
// drops events: we keep the producer non-blocking so one wedged
// consumer can't pin the watcher goroutine. Channels are not
// closed on Start exit; ctx cancellation stops the producer and
// the channels stay drainable.
func (w *Watcher) Subscribe() <-chan Change {
	ch := make(chan Change, 1)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

// Start primes the current Snapshot from a synchronous Detect and
// launches the polling goroutine. Idempotent: a second Start on
// the same Watcher is a no-op (the production caller is main.go,
// which only ever starts one).
func (w *Watcher) Start(ctx context.Context) {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	w.current = Detect().WithReserves(w.reserveCPU, w.reserveMem)
	w.started = true
	w.mu.Unlock()
	go w.loop(ctx)
}

func (w *Watcher) loop(ctx context.Context) {
	poll := time.NewTicker(w.pollEvery)
	defer poll.Stop()

	var (
		pending      *Snapshot
		debounceCh   <-chan time.Time
		debounceTime *time.Timer
	)
	cancelDebounce := func() {
		pending = nil
		if debounceTime != nil {
			debounceTime.Stop()
		}
		debounceTime = nil
		debounceCh = nil
	}

	for {
		select {
		case <-ctx.Done():
			cancelDebounce()
			return

		case <-poll.C:
			snap := Detect().WithReserves(w.reserveCPU, w.reserveMem)
			w.mu.Lock()
			current := w.current
			w.mu.Unlock()

			if exposedEqual(snap, current) {
				// Either nothing changed, or a brief excursion has
				// returned to baseline before we acted. Either way,
				// no pending commit.
				cancelDebounce()
				continue
			}
			if pending != nil && exposedEqual(snap, *pending) {
				// Same target as the one we are already waiting on;
				// keep the existing timer.
				continue
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
