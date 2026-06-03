//go:build !linux

package supervisor

// procStats is the cross-platform shape of per-process /proc data.
// Non-linux builds always return zero; macOS dev only sees the
// verbose path in unit tests and they don't depend on real values.
type procStats struct {
	RSSBytes   uint64
	CPUSeconds float64
	FDCount    int
}

func readProcStats(_ int) procStats { return procStats{} }
