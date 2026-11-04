//go:build !linux

package machine

import "time"

// KernelOOM is a stub off Linux: never reports an OOM kill.
type KernelOOM struct{}

func (KernelOOM) Now() time.Duration                       { return 0 }
func (KernelOOM) Killed(pid int, since time.Duration) bool { return false }
