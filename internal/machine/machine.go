// Package machine is zpinit's machine mode: what changes when zpinit is
// the kernel's own init (PID 1 of a microVM, or of anything booted with
// init=zpinit) rather than the init of a container.
//
// Machine mode is detected, never configured (IsKernelInit). Everything
// in this package is reached only behind that detection, so a container
// can never call reboot(2), open a vsock listener, or act on a planted
// machine.json.
package machine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/0ploy/zpinit/internal/config"
)

// SettingsPath is where the bringup init writes the machine settings
// before it execs zpinit.
const SettingsPath = "/run/zpinit/machine.json"

// initPIDNamespace is the link text of the kernel's initial PID
// namespace. PROC_PID_INIT_INO (0xEFFFFFFC) is a fixed inode; every
// other PID namespace gets a dynamically allocated one.
const initPIDNamespace = "pid:[4026531836]"

// IsKernelInit reports whether a process with this PID and this
// /proc/self/ns/pid link text is the kernel's init. Split from Detect so
// the predicate is testable without being PID 1.
func IsKernelInit(pid int, nsLink string) bool {
	return pid == 1 && nsLink == initPIDNamespace
}

// Detect reports whether this process is the kernel's init.
// /proc/self/ns/pid rather than /proc/1/ns/pid: the latter needs
// ptrace-read access to PID 1, the self link always works.
func Detect() bool {
	pid := os.Getpid()
	if pid != 1 {
		return false
	}
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return false
	}
	return IsKernelInit(pid, link)
}

// Version is the machine.json format version this zpinit reads.
const Version = 1

// Settings is /run/zpinit/machine.json. Its format is zpinit's; the
// bringup init that execs zpinit is the only writer.
type Settings struct {
	V int `json:"v"`
	// Console is where the reset line goes, even with logging off.
	// Empty means no reset line.
	Console string `json:"console,omitempty"`
	Stop    Stop   `json:"stop"`
	// User is the wrap-mode child's identity (the image USER,
	// user[:group], names or numeric ids). Empty in supervise mode and
	// means "root" in wrap mode.
	User string `json:"user,omitempty"`
	// Exec, when set, opens the vsock exec listener.
	Exec *Exec `json:"exec,omitempty"`
	// Rescue is set only when bringup failed and the machine is parked.
	Rescue *Rescue `json:"rescue,omitempty"`
}

// Stop is the graceful-stop contract for the whole machine.
type Stop struct {
	// Signal is what the wrap-mode child gets on a graceful stop (the
	// image STOPSIGNAL).
	Signal string `json:"signal"`
	// Timeout bounds the whole graceful stop; the host's own stop
	// timeout is longer.
	Timeout Duration `json:"timeout"`
}

// Exec configures the vsock exec listener.
type Exec struct {
	Port       uint32   `json:"port"`
	User       string   `json:"user,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	Env        []string `json:"env,omitempty"`
}

// Rescue says which bringup step failed.
type Rescue struct {
	Step  string `json:"step"`
	Error string `json:"error"`
}

// Duration is a time.Duration encoded as a Go duration string ("10s").
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// DefaultStop applies when machine.json is absent (a machine booted with
// init=zpinit and no bringup init in front).
var DefaultStop = Stop{Signal: "SIGTERM", Timeout: Duration(10 * time.Second)}

// Defaults are the settings of a machine with no machine.json: no reset
// line, no exec listener, not parked.
func Defaults() *Settings {
	return &Settings{V: Version, Stop: DefaultStop}
}

// Parse decodes and validates machine.json strictly: unknown fields,
// trailing data, an unknown version and invalid values are all errors.
// The caller falls back to rescue on any error rather than guessing,
// because a misread file could open or misconfigure the exec listener.
func Parse(b []byte) (*Settings, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Settings
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	// More() alone misses a stray closing bracket ("{...}}"); only EOF
	// after the object is clean.
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the settings object")
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Settings) validate() error {
	var errs []string
	if s.V != Version {
		errs = append(errs, fmt.Sprintf("v must be %d, got %d", Version, s.V))
	}
	if s.Console != "" && !consolePathOK(s.Console) {
		errs = append(errs, fmt.Sprintf("console %q must be a path under /dev/", s.Console))
	}
	if s.Stop.Signal == "" {
		errs = append(errs, "stop.signal is required")
	} else if _, ok := config.ParseSignal(s.Stop.Signal); !ok {
		errs = append(errs, fmt.Sprintf("stop.signal %q is not a recognised signal name", s.Stop.Signal))
	}
	if s.Stop.Timeout <= 0 {
		errs = append(errs, "stop.timeout must be > 0")
	}
	if e := s.Exec; e != nil {
		if e.Port == 0 || e.Port == 0xFFFFFFFF {
			errs = append(errs, fmt.Sprintf("exec.port %d is not a usable vsock port", e.Port))
		}
		if e.WorkingDir != "" && !strings.HasPrefix(e.WorkingDir, "/") {
			errs = append(errs, fmt.Sprintf("exec.working_dir %q must be absolute", e.WorkingDir))
		}
		for _, kv := range e.Env {
			if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
				errs = append(errs, fmt.Sprintf("exec.env entry %q is not KEY=value", kv))
				break
			}
		}
	}
	if r := s.Rescue; r != nil && r.Step == "" {
		errs = append(errs, "rescue.step is required when rescue is set")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// StopSignal returns the parsed stop.signal. Valid after Parse; falls
// back to SIGTERM for hand-built settings.
func (s *Settings) StopSignal() syscall.Signal {
	if sig, ok := config.ParseSignal(s.Stop.Signal); ok {
		return sig
	}
	return syscall.SIGTERM
}

// Load reads SettingsPath. A missing file yields Defaults and no error.
// Any other failure yields settings that park the machine (Rescue set,
// no exec listener) together with the error, so the caller can log it
// and still run: the machine stays up for inspection and a graceful
// stop ends it with cause=parked.
func Load(path string) (*Settings, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return parkedOn(nil, fmt.Errorf("read %s: %w", path, err)), err
	}
	s, err := Parse(b)
	if err != nil {
		err = fmt.Errorf("%s: %w", path, err)
		return parkedOn(b, err), err
	}
	return s, nil
}

// parkedOn builds the rescue settings for an unreadable machine.json.
// The console is salvaged from the raw bytes when it looks sane, so the
// host still gets a reset line; nothing else in a file that failed
// validation is trusted.
func parkedOn(raw []byte, err error) *Settings {
	s := Defaults()
	s.Rescue = &Rescue{Step: "zpinit", Error: err.Error()}
	var loose struct {
		Console string `json:"console"`
	}
	if raw != nil && json.Unmarshal(raw, &loose) == nil && consolePathOK(loose.Console) {
		s.Console = loose.Console
	}
	return s
}

// consolePathOK: a clean absolute path under /dev/. The reset line is
// written to it with O_WRONLY at offset 0, so "/dev/../etc/x" must not
// pass a prefix check.
func consolePathOK(p string) bool {
	return strings.HasPrefix(p, "/dev/") && len(p) > len("/dev/") && path.Clean(p) == p
}

// WarnIfPresent is the container-mode side of the gate: a machine.json
// in a container is never read, only reported.
func WarnIfPresent(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
