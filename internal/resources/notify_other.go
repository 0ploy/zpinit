//go:build !linux

package resources

import "log/slog"

// newCgroupNotifyPlatform has no implementation off Linux: inotify is
// Linux-only and macOS dev builds have no cgroupfs to watch anyway.
// Returning nil leaves the Watcher on its backstop poll, which is
// correct (just less prompt) and keeps `go build`/`go test` working on
// a developer laptop.
func newCgroupNotifyPlatform([]string, *slog.Logger) cgroupNotify { return nil }
