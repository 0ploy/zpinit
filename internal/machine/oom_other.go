//go:build !linux

package machine

import (
	"time"

	"github.com/0ploy/zpinit/internal/reaper"
)

// KernelOOM is a stub off Linux: never reports an OOM kill.
type KernelOOM struct{}

func (KernelOOM) Now() time.Duration { return 0 }

func (KernelOOM) Killed(cands []reaper.OOMCandidate) []bool { return make([]bool, len(cands)) }
