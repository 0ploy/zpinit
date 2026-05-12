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

	// Replicas is the number of independent copies of Command to
	// supervise. Default 1. Each replica is a first-class Runner with
	// its own PID, log file, and crash budget. ZPINIT_REPLICA_INDEX is
	// injected into each replica's env when Replicas > 1. Replicas of
	// an app that binds a port without SO_REUSEPORT support will
	// conflict on EADDRINUSE; see the README's "Node.js clustering"
	// section. `zpinit doctor` catches the common cases pre-boot.
	Replicas int `toml:"replicas"`
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
}
