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
func (rt *realTimer) Stop()                  { rt.t.Stop() }
