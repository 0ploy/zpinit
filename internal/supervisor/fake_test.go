package supervisor

import (
	"sync"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/reaper"
)

// fakeClock is a manually-advanced clock for deterministic tests.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) NewTimer(d time.Duration) Timer {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	t := &fakeTimer{
		fireAt: fc.now.Add(d),
		ch:     make(chan time.Time, 1),
	}
	fc.timers = append(fc.timers, t)
	return t
}

// Advance moves the clock forward by d, firing any timers whose
// deadline has passed.
func (fc *fakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	fc.now = fc.now.Add(d)
	for _, t := range fc.timers {
		if !t.stopped && !t.fired && !fc.now.Before(t.fireAt) {
			t.fired = true
			select {
			case t.ch <- fc.now:
			default:
			}
		}
	}
	fc.mu.Unlock()
}

type fakeTimer struct {
	fireAt  time.Time
	ch      chan time.Time
	stopped bool
	fired   bool
}

func (t *fakeTimer) Chan() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop()                  { t.stopped = true }

// fakeProcess is a fake Process whose Exit is driven by the test.
type fakeProcess struct {
	pid       int
	exit      chan reaper.ExitInfo
	signalsMu sync.Mutex
	signals   []syscall.Signal
}

func newFakeProcess(pid int) *fakeProcess {
	return &fakeProcess{
		pid:  pid,
		exit: make(chan reaper.ExitInfo, 1),
	}
}

func (p *fakeProcess) PID() int                     { return p.pid }
func (p *fakeProcess) Exit() <-chan reaper.ExitInfo { return p.exit }
func (p *fakeProcess) SignalGroup(s syscall.Signal) error {
	p.signalsMu.Lock()
	p.signals = append(p.signals, s)
	p.signalsMu.Unlock()
	return nil
}

func (p *fakeProcess) signalsReceived() []syscall.Signal {
	p.signalsMu.Lock()
	defer p.signalsMu.Unlock()
	out := make([]syscall.Signal, len(p.signals))
	copy(out, p.signals)
	return out
}

// pushExit simulates the kernel reaping this process.
func (p *fakeProcess) pushExit(info reaper.ExitInfo) {
	info.PID = p.pid
	p.exit <- info
	close(p.exit)
}
