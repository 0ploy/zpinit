// Package config loads and validates zpinit configuration: a globals file
// (zpinit.toml) plus a directory of service files (services/*.toml).
//
// All defaults are applied during Load; downstream code can rely on the
// returned struct having concrete values for every field.
package config

type Restart string

const (
	RestartAlways    Restart = "always"
	RestartOnFailure Restart = "on-failure"
	RestartNever     Restart = "never"
)

type EntrypointOnFailure string

const (
	EntrypointFail     EntrypointOnFailure = "fail"
	EntrypointContinue EntrypointOnFailure = "continue"
)

type ReadyOnTimeout string

const (
	ReadyFail     ReadyOnTimeout = "fail"
	ReadyContinue ReadyOnTimeout = "continue"
)

type Globals struct {
	EntrypointOnFailure     EntrypointOnFailure `toml:"entrypoint_on_failure"`
	EntrypointScriptTimeout Duration            `toml:"entrypoint_script_timeout"`
	BootTimeout             Duration            `toml:"boot_timeout"`
	DefaultStopSignal       string              `toml:"default_stop_signal"`
	DefaultStopTimeout      Duration            `toml:"default_stop_timeout"`
	ExitCodeFrom            string              `toml:"exit_code_from"`
	ControlSocket           string              `toml:"control_socket"`
	// Env is the fleet-wide default env map for this image. Lowest
	// precedence: container env (Dockerfile ENV, docker run -e) and
	// entrypoint.d-set vars both override it. Visible to the wrap-mode
	// CMD and to supervise-mode services, but NOT to docker exec, since
	// it travels via the syscall.Exec / spawn env path rather than the
	// container's stored config.
	Env map[string]string `toml:"env"`
	// Resources holds operator-configured reservations and (later)
	// autoscaler tuning. All fields default to zero; an absent
	// [resources] block is identical to all defaults.
	Resources Resources `toml:"resources"`
}

// Resources is the [resources] section of zpinit.toml. ReserveCPU is
// subtracted from the detected CPU budget before children see
// ZPINIT_CPU_COUNT / ZPINIT_CPU_QUOTA; ReserveMemory is subtracted
// from ZPINIT_MEMORY_BYTES. ScaleUpAfter / ScaleDownAfter set the
// per-direction debounce for the live resource watcher: a change has
// to hold for the configured duration before it is committed (and
// reload_on_change services are reloaded). Defaults: 5 s up, 30 s
// down — eager scale-up, patient scale-down.
type Resources struct {
	ReserveCPU     float64  `toml:"reserve_cpu"`
	ReserveMemory  ByteSize `toml:"reserve_memory"`
	ScaleUpAfter   Duration `toml:"scale_up_after"`
	ScaleDownAfter Duration `toml:"scale_down_after"`
}

type Logging struct {
	Stdout string `toml:"stdout"`
	Stderr string `toml:"stderr"`
}

type Ready struct {
	Command   []string       `toml:"command"`
	Interval  Duration       `toml:"interval"`
	Timeout   Duration       `toml:"timeout"`
	OnTimeout ReadyOnTimeout `toml:"on_timeout"`
}

type Service struct {
	// Filename is the on-disk basename (e.g. "10_mysql.toml"). It determines
	// start order. Set by the loader, not from TOML.
	Filename string `toml:"-"`

	Name              string   `toml:"name"`
	Command           []string `toml:"command"`
	Cwd               string   `toml:"cwd"`
	User              string   `toml:"user"`
	Group             string   `toml:"group"`
	Restart           Restart  `toml:"restart"`
	BackoffInitial    Duration `toml:"backoff_initial"`
	BackoffMax        Duration `toml:"backoff_max"`
	BackoffResetAfter Duration `toml:"backoff_reset_after"`
	StopSignal        string   `toml:"stop_signal"`
	StopTimeout       Duration `toml:"stop_timeout"`
	// Reloadable defaults to true. Pointer so we can distinguish "unset"
	// from "explicitly false".
	Reloadable *bool             `toml:"reloadable"`
	Env        map[string]string `toml:"env"`
	Log        Logging           `toml:"log"`
	Ready      *Ready            `toml:"ready"`

	// Replicas declares the number of independent copies of Command
	// to supervise. Accepts an integer (`replicas = 3`) or the
	// string `"auto"` (`replicas = "auto"`). For static counts each
	// replica is a first-class Runner with its own PID, log file,
	// and crash budget; ZPINIT_REPLICA_INDEX is injected when N>1.
	// For auto, zpinit computes the count from the detected CPU
	// budget at boot and updates it on every watcher-driven resource
	// change, clamped to [ReplicasMin, ReplicasMax] when set.
	// Replicas of an app that binds a port without SO_REUSEPORT
	// will conflict on EADDRINUSE; see the README's "Node.js
	// clustering" section. `zpinit doctor` catches common cases
	// pre-boot.
	Replicas Replicas `toml:"replicas"`

	// ReplicasMin / ReplicasMax bound the auto-computed replica
	// count. Both optional. Default min = 1 (zpinit always runs at
	// least one replica). Default max = unbounded — the natural
	// CPU-derived count caps unless overridden. Setting min > the
	// natural count is the way to express "more than CPU count for
	// I/O-bound workloads" (a queue worker that should run 16 even
	// on a 2-CPU host).
	ReplicasMin int `toml:"replicas_min"`
	ReplicasMax int `toml:"replicas_max"`

	// ReloadSignal, if set, replaces the default stop+start cycle of
	// `zpctl reload <name>` with an in-place signal to the service's
	// process group. Use for apps that re-read their config on a
	// known signal (nginx HUP, php-fpm USR2). Mutually exclusive with
	// ReloadCommand.
	ReloadSignal string `toml:"reload_signal"`

	// ReloadCommand, if set, replaces the default stop+start cycle of
	// `zpctl reload <name>` with a one-shot command that talks to the
	// live process via its own IPC (e.g. `nginx -s reload`). The
	// command inherits the service's env; stdout/stderr land in the
	// service log; non-zero exit is logged but does not kill the
	// service. Mutually exclusive with ReloadSignal.
	ReloadCommand []string `toml:"reload_command"`

	// ReloadOnChange lists the resource dimensions that, when their
	// exposed value moves (ZPINIT_CPU_COUNT for "cpu",
	// ZPINIT_MEMORY_BYTES for "memory"), cause zpinit to perform a
	// reload action on this service. The action is whatever
	// ReloadSignal/ReloadCommand declares, falling back to full
	// restart if neither is set. Empty or nil disables the trigger.
	ReloadOnChange []string `toml:"reload_on_change"`
}

// IsReloadable returns true unless the service explicitly set
// reloadable=false. Value receiver so callers holding a Service
// (rather than *Service) can call it directly.
func (s Service) IsReloadable() bool {
	return s.Reloadable == nil || *s.Reloadable
}

type Config struct {
	Dir      string
	Globals  Globals
	Services []Service // sorted by Filename
	Warnings []string  // non-fatal issues collected during load
	// SkippedFiles records service files that failed to parse or
	// validate and were left out of Services. Per-file isolation:
	// one malformed file does not abort the whole load, so the valid
	// files still appear in Services. Callers report these and decide
	// the exit code (zpctl/--check-config exit non-zero; daemon boot
	// logs and continues). Ordered by filename, matching the load walk.
	SkippedFiles []FileError
}

// FileError pairs a service file's basename with the parse or
// validation error that caused the loader to skip it. File is the
// on-disk basename (e.g. "0050_apache2.toml"), which is also the diff
// key, so the supervisor can match a skipped file against a running
// runner and leave it untouched rather than tearing it down.
type FileError struct {
	File string
	Err  error
}

func (fe FileError) Error() string { return fe.File + ": " + fe.Err.Error() }
