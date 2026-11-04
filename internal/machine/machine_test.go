package machine

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIsKernelInit(t *testing.T) {
	cases := []struct {
		pid  int
		link string
		want bool
	}{
		{1, "pid:[4026531836]", true},
		// A container's init: PID 1 in a namespace of its own.
		{1, "pid:[4026532817]", false},
		// --pid=host, or under another init: not PID 1.
		{4242, "pid:[4026531836]", false},
		{1, "", false},
	}
	for _, c := range cases {
		if got := IsKernelInit(c.pid, c.link); got != c.want {
			t.Errorf("IsKernelInit(%d, %q) = %v, want %v", c.pid, c.link, got, c.want)
		}
	}
}

func TestDetectFalseInTests(t *testing.T) {
	// The test binary is never the kernel's init; if this ever flips,
	// machine-mode code would run (and reboot) on a developer's host.
	if Detect() {
		t.Fatal("Detect() reported machine mode for a test process")
	}
}

// wrapHandoff is a complete machine.json for a wrap-mode handoff, as a
// bringup init writes it: the published format, field for field.
const wrapHandoff = `{"v":1,"console":"/dev/ttyS0","stop":{"signal":"SIGQUIT","timeout":"10s"},"user":"nginx",` +
	`"exec":{"port":1024,"user":"app","working_dir":"/srv","env":["PATH=/bin","HOME=/home/nginx"]}}`

func TestParseWrapHandoff(t *testing.T) {
	s, err := Parse([]byte(wrapHandoff))
	if err != nil {
		t.Fatal(err)
	}
	if s.Console != "/dev/ttyS0" || s.User != "nginx" || s.Rescue != nil {
		t.Errorf("unexpected settings: %+v", s)
	}
	if s.StopSignal() != syscall.SIGQUIT || s.Stop.Timeout.Std() != 10*time.Second {
		t.Errorf("stop = %+v", s.Stop)
	}
	if s.Exec == nil || s.Exec.Port != 1024 || s.Exec.User != "app" || s.Exec.WorkingDir != "/srv" || len(s.Exec.Env) != 2 {
		t.Errorf("exec = %+v", s.Exec)
	}
}

func TestParseRescue(t *testing.T) {
	in := `{"v":1,"console":"/dev/ttyS0","stop":{"signal":"SIGTERM","timeout":"10s"},` +
		`"rescue":{"step":"network","error":"interface eth0: no link"}}`
	s, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if s.Rescue == nil || s.Rescue.Step != "network" || s.Exec != nil {
		t.Errorf("unexpected settings: %+v", s)
	}
}

func TestParseRejects(t *testing.T) {
	base := `"console":"/dev/ttyS0","stop":{"signal":"SIGTERM","timeout":"10s"}`
	cases := map[string]string{
		"unknown field":   `{"v":1,` + base + `,"surprise":true}`,
		"unknown nested":  `{"v":1,` + base + `,"exec":{"port":1,"shell":"/bin/sh"}}`,
		"version":         `{"v":2,` + base + `}`,
		"no version":      `{` + base + `}`,
		"trailing":        `{"v":1,` + base + `} {}`,
		"trailing brace":  `{"v":1,` + base + `}}`,
		"trailing square": `{"v":1,` + base + `}]`,
		"console escape":  `{"v":1,"console":"/dev/../etc/x","stop":{"signal":"SIGTERM","timeout":"1s"}}`,
		"console bare":    `{"v":1,"console":"/dev/","stop":{"signal":"SIGTERM","timeout":"1s"}}`,
		"bad signal":      `{"v":1,"stop":{"signal":"SIGNOPE","timeout":"10s"}}`,
		"no signal":       `{"v":1,"stop":{"timeout":"10s"}}`,
		"zero timeout":    `{"v":1,"stop":{"signal":"SIGTERM","timeout":"0s"}}`,
		"numeric timeout": `{"v":1,"stop":{"signal":"SIGTERM","timeout":10}}`,
		"console path":    `{"v":1,"console":"/etc/passwd","stop":{"signal":"SIGTERM","timeout":"1s"}}`,
		"port zero":       `{"v":1,` + base + `,"exec":{"port":0}}`,
		"relative wd":     `{"v":1,` + base + `,"exec":{"port":5,"working_dir":"srv"}}`,
		"env entry":       `{"v":1,` + base + `,"exec":{"port":5,"env":["NOEQUALS"]}}`,
		"rescue no step":  `{"v":1,` + base + `,"rescue":{"error":"x"}}`,
		"not json":        `v=1`,
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: Parse accepted %s", name, in)
		}
	}
}

func TestLoadMissingIsDefaults(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "machine.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Rescue != nil || s.Exec != nil || s.Console != "" {
		t.Errorf("defaults = %+v", s)
	}
	if s.StopSignal() != syscall.SIGTERM || s.Stop.Timeout.Std() != 10*time.Second {
		t.Errorf("default stop = %+v", s.Stop)
	}
}

func TestLoadInvalidParks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "machine.json")
	// Valid JSON with an unknown field and an exec block: the machine
	// must park, keep the console for the reset line, and open no
	// exec listener on settings it could not validate.
	in := `{"v":1,"console":"/dev/ttyS1","stop":{"signal":"SIGTERM","timeout":"5s"},"exec":{"port":1024},"extra":1}`
	if err := os.WriteFile(p, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err == nil {
		t.Fatal("Load accepted an invalid file")
	}
	if s.Rescue == nil || s.Rescue.Step != "zpinit" || !strings.Contains(s.Rescue.Error, "extra") {
		t.Errorf("rescue = %+v", s.Rescue)
	}
	if s.Exec != nil {
		t.Error("exec listener configured from an invalid file")
	}
	if s.Console != "/dev/ttyS1" {
		t.Errorf("console = %q, want salvaged /dev/ttyS1", s.Console)
	}

	// Garbage: still parks, no console.
	if err := os.WriteFile(p, []byte("\x00garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = Load(p)
	if err == nil || s.Rescue == nil || s.Console != "" {
		t.Errorf("garbage: err=%v settings=%+v", err, s)
	}
}

func TestResetLine(t *testing.T) {
	cases := []struct {
		o    Outcome
		want string
	}{
		{Outcome{Cause: CauseExit, Code: 0}, "\nzpinit: reset intent=shutdown cause=exit status=0\n"},
		{Outcome{Cause: CauseExit, Code: 139, Signal: syscall.SIGSEGV}, "\nzpinit: reset intent=shutdown cause=exit signal=SIGSEGV\n"},
		{Outcome{Cause: CauseStop, Code: 143}, "\nzpinit: reset intent=shutdown cause=stop status=143\n"},
		{Outcome{Cause: CauseStop, Code: 143, Signal: syscall.SIGTERM}, "\nzpinit: reset intent=shutdown cause=stop signal=SIGTERM\n"},
		{Outcome{Cause: CauseParked}, "\nzpinit: reset intent=shutdown cause=parked\n"},
		{Outcome{Cause: CauseExit, Code: 162, Signal: syscall.Signal(34)}, "\nzpinit: reset intent=shutdown cause=exit signal=SIG34\n"},
	}
	for _, c := range cases {
		if got := ResetLine(c.o); got != c.want {
			t.Errorf("ResetLine(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}

// TestResetLineFitsHostBuffer: the reset line promises to stay under
// 1024 bytes, because a host's serial reader may drop longer lines on
// purpose (a guest must not make the host
// allocate), so a longer reset line would make the reset look
// unexplained. Every field is bounded; check the worst cases.
func TestResetLineFitsHostBuffer(t *testing.T) {
	const hostLineLimit = 1024
	worst := []Outcome{
		{Cause: CauseStop, Code: -2147483648},
		{Cause: CauseExit, Code: 2147483647},
		{Cause: CauseExit, Code: 192, Signal: syscall.Signal(64)},
		{Cause: CauseExit, Code: 154, Signal: syscall.SIGVTALRM},
		{Cause: CauseParked},
	}
	for _, o := range worst {
		line := strings.TrimPrefix(ResetLine(o), "\n")
		if len(line) >= hostLineLimit {
			t.Errorf("reset line %d bytes: %q", len(line), line)
		}
		if !strings.HasPrefix(line, ResetLinePrefix+"intent=shutdown ") {
			t.Errorf("reset line %q lacks the agreed prefix and intent", line)
		}
	}
}
