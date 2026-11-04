//go:build !linux

package machine

import (
	"errors"
	"log/slog"
	"os"
)

// DisableCAD is Linux-only; machine mode never triggers elsewhere.
func DisableCAD() error { return errors.New("machine mode is Linux-only") }

// End is Linux-only. Machine mode cannot be detected off Linux, so this
// only keeps the build working on macOS dev machines.
func End(_ *slog.Logger, _ *Settings, o Outcome) {
	os.Exit(o.Code)
}
