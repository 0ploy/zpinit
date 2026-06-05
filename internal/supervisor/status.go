package supervisor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// statusJSON is the machine-readable shape emitted by `zpctl status
// --json`, one object per target (each replica is its own object).
// Pointer fields render as JSON null when not applicable so a consumer
// can branch on presence rather than sentinel values.
type statusJSON struct {
	Name          string        `json:"name"`           // display name, includes /N for replicas
	Service       string        `json:"service"`        // logical service name (no /N)
	ReplicaIndex  *int          `json:"replica_index"`  // null when the service is not replicated
	State         string        `json:"state"`          // supervisord state string
	PID           *int          `json:"pid"`            // null when no live process
	UptimeSeconds *int64        `json:"uptime_seconds"` // null unless currently RUNNING
	TotalSpawns   int           `json:"total_spawns"`
	LastExit      *lastExitJSON `json:"last_exit"` // null until the service has exited at least once
	// /proc-derived fields, present only with --verbose and a live PID.
	RSSBytes   *uint64  `json:"rss_bytes,omitempty"`
	CPUSeconds *float64 `json:"cpu_seconds,omitempty"`
	FDs        *int     `json:"fds,omitempty"`
}

// lastExitJSON carries exactly one of code/signal (whichever the last
// reaped exit produced); the unused field is omitted.
type lastExitJSON struct {
	Code   *int    `json:"code,omitempty"`
	Signal *string `json:"signal,omitempty"`
}

// formatStatusJSON renders one runner as a compact single-line JSON
// object. Mirrors the data formatStatusLine/Verbose surface, but in a
// stable machine-readable shape. Marshal of these plain types cannot
// fail; on the impossible error we emit a minimal object so the line
// is still valid JSON.
func formatStatusJSON(r *Runner, verbose bool) string {
	snap := r.Snapshot()
	cfg := r.Cfg()

	out := statusJSON{
		Name:        r.DisplayName(),
		Service:     cfg.Name,
		State:       mapToSupervisordState(snap.State, snap.Manual),
		TotalSpawns: snap.TotalSpawns,
	}
	if cfg.Replicas.N > 1 || cfg.Replicas.Auto {
		idx := r.ReplicaIndex()
		out.ReplicaIndex = &idx
	}
	if snap.PID > 0 {
		pid := snap.PID
		out.PID = &pid
	}
	if snap.State == StateRunning && !snap.UpSince.IsZero() {
		up := int64(time.Since(snap.UpSince).Seconds())
		out.UptimeSeconds = &up
	}
	if le := snap.LastExit; le.PID != 0 {
		out.LastExit = &lastExitJSON{}
		if le.Signaled {
			sig := le.Signal.String()
			out.LastExit.Signal = &sig
		} else {
			code := le.ExitCode
			out.LastExit.Code = &code
		}
	}
	if verbose && snap.PID > 0 {
		ps := readProcStats(snap.PID)
		rss := ps.RSSBytes
		cpu := ps.CPUSeconds
		fds := ps.FDCount
		out.RSSBytes = &rss
		out.CPUSeconds = &cpu
		out.FDs = &fds
	}

	b, err := json.Marshal(out)
	if err != nil {
		return fmt.Sprintf(`{"name":%q,"state":%q}`, out.Name, out.State)
	}
	return string(b)
}

func formatStatusLine(r *Runner) string {
	snap := r.Snapshot()
	state := mapToSupervisordState(snap.State, snap.Manual)
	return fmt.Sprintf("%-32s %-9s %s", r.DisplayName(), state, stateDetail(snap))
}

// formatStatusLineVerbose returns the verbose status row: the
// regular state line plus key=value pairs for the data operators
// typically reach for during triage but otherwise have to assemble
// from `cat /proc/$(zpctl pid svc)/status` and `zpctl status` runs.
// Pure read; no side effects, no rate-limiting (this is a human-
// driven command, not a polling target).
//
// RSS/CPU/FDs come from /proc and are only meaningful when the
// service is actually running with a PID; the formatter prints them
// only in that case. last_exit / spawns are always meaningful.
func formatStatusLineVerbose(r *Runner) string {
	snap := r.Snapshot()
	state := mapToSupervisordState(snap.State, snap.Manual)
	base := fmt.Sprintf("%-32s %-9s %s", r.DisplayName(), state, stateDetail(snap))

	var extras []string
	if snap.PID > 0 {
		ps := readProcStats(snap.PID)
		if ps.RSSBytes > 0 {
			extras = append(extras, fmt.Sprintf("rss=%s", formatBytes(ps.RSSBytes)))
		}
		if ps.CPUSeconds > 0 {
			extras = append(extras, fmt.Sprintf("cpu=%s", formatCPU(ps.CPUSeconds)))
		}
		if ps.FDCount > 0 {
			extras = append(extras, fmt.Sprintf("fds=%d", ps.FDCount))
		}
	}
	extras = append(extras, fmt.Sprintf("spawns=%d", snap.TotalSpawns))
	if le := snap.LastExit; le.PID != 0 {
		if le.Signaled {
			extras = append(extras, fmt.Sprintf("last_exit=signal:%s", le.Signal.String()))
		} else {
			extras = append(extras, fmt.Sprintf("last_exit=code:%d", le.ExitCode))
		}
	}
	return base + "  " + strings.Join(extras, " ")
}

// formatBytes renders a byte count for verbose status. Picks the
// biggest unit that yields a value >= 1 (using binary 1024-based
// units to match what /proc reports).
func formatBytes(n uint64) string {
	const (
		Ki = uint64(1) << 10
		Mi = uint64(1) << 20
		Gi = uint64(1) << 30
	)
	switch {
	case n >= Gi:
		return fmt.Sprintf("%.1fGiB", float64(n)/float64(Gi))
	case n >= Mi:
		return fmt.Sprintf("%.1fMiB", float64(n)/float64(Mi))
	case n >= Ki:
		return fmt.Sprintf("%.1fKiB", float64(n)/float64(Ki))
	}
	return fmt.Sprintf("%dB", n)
}

// formatCPU renders accumulated CPU seconds as Hh:Mm:Ss or Mm:Ss,
// matching supervisord-style readability.
func formatCPU(secs float64) string {
	totalSecs := int(secs)
	h := totalSecs / 3600
	m := (totalSecs % 3600) / 60
	s := totalSecs % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func mapToSupervisordState(s State, manualStop bool) string {
	switch s {
	case StatePending:
		return "STOPPED"
	case StateStarting:
		return "STARTING"
	case StateRunning:
		return "RUNNING"
	case StateBackoff:
		return "BACKOFF"
	case StateStopping:
		return "STOPPING"
	case StateStopped:
		if manualStop {
			return "STOPPED"
		}
		return "EXITED"
	case StateFatal:
		return "FATAL"
	}
	return "UNKNOWN"
}

// stateDetail renders the per-state suffix from a single Runner
// Snapshot, so the "RUNNING pid 0" / "RUNNING pid X, uptime 0s"
// race window that sequential per-field accessors expose can't
// happen. DisplayName isn't on Status because it's derived from
// immutable Runner fields and doesn't need the lock.
func stateDetail(snap Status) string {
	switch snap.State {
	case StateRunning:
		if snap.UpSince.IsZero() {
			return fmt.Sprintf("pid %d", snap.PID)
		}
		return fmt.Sprintf("pid %d, uptime %s", snap.PID, formatUptime(time.Since(snap.UpSince)))
	case StateBackoff:
		return fmt.Sprintf("backoff (crashes %d)", snap.Crashes)
	case StateFatal:
		return "Exited too quickly (process log may have details)"
	case StateStarting:
		return "starting"
	case StateStopping:
		return "stopping"
	}
	return ""
}

// formatUptime renders a duration as H:MM:SS (or Dd HH:MM if it's been
// up for more than a day), matching supervisorctl status output.
func formatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSecs := int(d.Seconds())
	days := totalSecs / 86400
	hours := (totalSecs % 86400) / 3600
	mins := (totalSecs % 3600) / 60
	secs := totalSecs % 60
	if days > 0 {
		return fmt.Sprintf("%dd %02d:%02d", days, hours, mins)
	}
	return fmt.Sprintf("%d:%02d:%02d", hours, mins, secs)
}
