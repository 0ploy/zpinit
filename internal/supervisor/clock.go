package supervisor

import "time"

// Clock abstracts time so the state machine can be unit-tested with a
// deterministic fake. The real clock just delegates to the time package.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer mirrors time.Timer's surface but allows fake implementations.
type Timer interface {
	Chan() <-chan time.Time
	Stop()
}

type realClock struct{}

// RealClock returns a Clock backed by time.Now and time.NewTimer.
func RealClock() Clock { return realClock{} }

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (rt *realTimer) Chan() <-chan time.Time { return rt.t.C }

// Stop does not drain rt.t.C. That is safe ONLY because realTimer is
// used single-shot: the Run loop nils its timer slot before handling a
// fire and recomputes the channel each iteration, so a stale tick can
// never be re-selected. (Under the go.mod 1.26 floor, Go 1.23+ timer
// semantics also mean Stop needs no manual drain.) Do not Stop() then
// re-read Chan() on the same realTimer; construct a new one instead.
func (rt *realTimer) Stop() { rt.t.Stop() }
