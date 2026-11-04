# Machine mode

zpinit can be PID 1 of a whole machine, not just of a container: the
kernel's own init. The case it is built for is a Firecracker microVM,
where a one-shot **bringup init** mounts the disk, configures the
network, writes the files below and `execve`s zpinit:

```
kernel -> bringup init (one-shot) -> execve -> zpinit (PID 1 for the life of the VM) -> workload
```

Anything booted with `init=zpinit` gets the same behaviour. zpinit
stays free of OCI and hypervisor knowledge: image-derived settings
reach it as argv, environment and its normal config; the few settings
only the VM side knows arrive in one file, `machine.json`.

> **Status:** implemented, unit and container tested, and verified in
> Firecracker v1.17.0 microVMs (guest kernel 6.18.39) on 2026-11-03:
> supervise mode, wrap mode with real images (nginx, redis, postgres,
> python, alpine), each image's stop signal, Ctrl+Alt+Del, exit-status
> propagation, rescue and the reset line on every ending. Exec over
> vsock is unit tested but not yet exercised by a host client in a
> microVM.

## Detection

zpinit is in machine mode when

```
getpid() == 1  &&  readlink("/proc/self/ns/pid") == "pid:[4026531836]"
```

`4026531836` is `PROC_PID_INIT_INO`, the fixed inode of the kernel's
initial PID namespace; every other PID namespace gets a dynamic one.
There is **no flag and no environment variable**: detection is the only
switch, so nobody can turn machine mode on in a container by mistake.

| Deployment | Context |
| --- | --- |
| Docker, Podman, LXC, Kubernetes | container: own PID namespace |
| `--pid=host` container, zpinit under tini/systemd | container: not PID 1 |
| Kata, firecracker-containerd | container: the workload runs in a PID namespace inside the guest |
| microVM, bringup init -> `execve` zpinit | **machine** |
| any machine booted with `init=zpinit` | **machine** |

The boot banner and the first log line carry `context=container` or
`context=machine`.

## Which mode runs

Machine mode is a context, not a fourth mode. The mode still comes from
the CMD, exactly as in a container:

| argv from the bringup init | Mode | What runs |
| --- | --- | --- |
| `[zpinit, "--", cmd...]` | wrap | entrypoint.d, then the CMD as a child of zpinit (the resident wrap of `--init`, forced), as `machine.json`'s `user`. The CMD ending ends the machine. |
| `[zpinit]` | supervise | entrypoint.d, then the services from the image's own `/etc/zpinit`, with the control socket and `zpctl` as in a container. The machine ends on a stop, on `zpctl shutdown`, when `exit_code_from` fires, or when a service fails its initial boot (`on_boot_failure = "fail"`, `cause=exit status=1`). |
| anything, with `rescue` in `machine.json` | rescue | nothing from the image; see [Rescue](#rescue). |

Wrap mode never execs in machine mode: the CMD replacing PID 1 would
mean the workload's exit panics the kernel. A `user` that does not
resolve ends the machine with `cause=exit status=126`; it does not
park.

In supervise mode zpinit warns at boot when the services' worst-case
stop exceeds `stop.timeout`. Service files stop one after another, and
each counts `stop_timeout` plus 5 s of reap grace, so with the defaults
(10 s per service, 10 s for the machine) a single service already
warns; raise `stop.timeout` or lower `stop_timeout`. The check runs at
boot only, not after a reload. A stop that hits the cutoff logs
`orchestrator did not return within budget` and ends with `cause=stop
status=1`.

## What changes

| | Container | Machine |
| --- | --- | --- |
| Ctrl+Alt+Del | n/a | `reboot(CAD_OFF)` is zpinit's first act, so the kernel delivers it as `SIGINT`: a graceful stop |
| `SIGUSR1`, `SIGUSR2` to zpinit | forwarded to an `--init` CMD, otherwise discarded | a graceful stop (busybox `halt`, `poweroff`; see [below](#reboot-halt-and-poweroff-inside-the-machine)) |
| Wrap mode | exec (fork with `--init`) | always fork; the CMD runs as `machine.json`'s `user`, zpinit stays root |
| Wrap-mode stop | the received signal, `default_stop_timeout` | `stop.signal`, `stop.timeout` |
| Supervise-mode stop | budget from the services' stop timeouts | the same, cut off at `stop.timeout` |
| Ending | `exit(code)` | teardown, sync, reset line, `reboot` |
| `machine.json` | warned about, never read | read |
| A tracked process OOM-killed | not distinguished from any `SIGKILL` | reported ([below](#out-of-memory-kills)) |
| vsock exec | never | when `machine.json` has `exec` |
| No cgroup limits | warning: figures may be the host's | expected: vCPUs and RAM are the budget |

Every other behaviour (config format, services, entrypoint.d, zpctl) is
the container one.

## `/run/zpinit/machine.json`

Written by the bringup init before the `execve`. It should be mode
0600, because `exec.env` routinely carries credentials; zpinit does not
check the mode or owner. Read only in machine mode.

```jsonc
{
  "v": 1,
  "console": "/dev/ttyS0",                 // where the reset line goes; absent = no reset line
  "stop": { "signal": "SIGQUIT",           // the image STOPSIGNAL, sent to the wrap-mode CMD
            "timeout": "10s" },            // the whole graceful stop; the host's own timeout is longer
  "user": "nginx",                         // the wrap-mode CMD's identity, user[:group]; absent = root
  "exec": {                                // absent = no exec listener
    "port": 1024,
    "user": "nginx",                       // default exec user (the image USER)
    "working_dir": "/",
    "env": ["PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/home/nginx"]  // base env of every exec
  },
  "rescue": { "step": "network", "error": "..." }   // present only when bringup failed
}
```

- **Strict.** All of these are errors:
  - unknown fields, trailing data, a version other than 1;
  - a missing `stop.signal` or `stop.timeout` (both are required when
    the file exists), a `stop.timeout` that is not a duration string
    (`"10s"`) or not positive;
  - a signal that is not a standard name, with or without `SIG`. Numeric
    values (`"9"`) and realtime names (`SIGRTMIN+3`) are not accepted,
    so the bringup init translates a numeric image STOPSIGNAL first;
  - a `console` that is not a clean path under `/dev/`;
  - an exec port of 0 or `0xFFFFFFFF`, a relative `exec.working_dir`,
    an `exec.env` entry that is not `KEY=value`;
  - `rescue` without `rescue.step`.
- **An unusable file parks the machine** (rescue with
  `step=zpinit`) rather than guessing. No exec listener is opened from
  a file that failed validation; only the console path is salvaged so
  the reset line still reaches the host.
- **No file** means defaults: no reset line, no exec listener,
  `SIGTERM` with a 10 s budget.

Alongside it the bringup init hands over: argv (`[zpinit]` for supervise mode,
`[zpinit, "--", cmd...]` for wrap mode), the composed environment as
`envp` and again in `/run/zpinit/env` for entrypoint.d, the working
directory, and stdio (fd 0 `/dev/null`, fds 1 and 2 the serial console
or `/dev/null`).

## Ending the machine

When the kernel's init exits, the kernel panics (`Attempted to kill
init!`) and unsynced writes are lost. So in machine mode every way out
of zpinit (the workload ending, a graceful stop, `exit_code_from`, a
config error, entrypoint.d failing, a panic on the main goroutine) runs
the same sequence:

1. the normal graceful teardown (services in reverse filename order, or
   the wrap CMD with `stop.signal` then `SIGKILL` at `stop.timeout`);
2. `kill(-1, SIGKILL)` for everything left (orphans, exec sessions,
   daemonised strays), reaping until `ECHILD` (bounded at 5 s for
   processes stuck in uninterruptible sleep);
3. `sync()`;
4. every other block-backed mount unmounted in reverse mount order
   (falling back to a read-only remount, then a lazy unmount), then `/`
   remounted read-only. zpinit runs from a tmpfs and holds nothing open
   on the disk, so the remount succeeds once everything else is dead;
   if it does not, the journal still leaves the disk consistent;
5. `sync()` again;
6. the reset line on the console;
7. `reboot(LINUX_REBOOT_CMD_RESTART)`. Firecracker runs guests with
   `reboot=k`, and a guest reset ends the VMM. If `reboot` fails, PID 1
   sleeps forever; it never returns.

A panic on a goroutine other than main cannot be recovered; the kernel
then panics, and with `panic=1` on the command line the VM still ends,
with no reset line.

None of this can run in a container. There `reboot(RESTART)` from a
namespace's init goes through `reboot_pid_ns()`, which kills the init
and makes the container exit as killed by `SIGHUP`, changing the exit
status every orchestrator keys on.

## Reboot, halt and poweroff inside the machine

A Firecracker guest cannot reboot, halt or power off in place: a reset
ends the VMM, and a halted guest just sits there. So from inside the
machine there is one way down, a graceful stop, and whether the
machine comes back is the host's restart policy, the same as a
container whose process exits.

| Typed inside the machine | Reaches zpinit as | Result |
| --- | --- | --- |
| busybox `reboot` | `SIGTERM` | graceful stop, `cause=stop` |
| busybox `halt`, `poweroff` | `SIGUSR1`, `SIGUSR2` | graceful stop, `cause=stop` |
| `zpctl shutdown` (supervise mode) | the control socket | graceful stop, `cause=stop` |
| `kill 1` | `SIGTERM` | graceful stop, `cause=stop` |
| systemd's `reboot`, `poweroff`, `halt` (symlinks to `systemctl`) | nothing | `System has not been booted with systemd as init system`; nothing happens |
| `reboot -f`, `poweroff -f` | nobody: `reboot(2)` directly | the machine resets at once: no graceful stop, no reset line |

systemd's tools talk to systemd over D-Bus, so zpinit cannot answer
them. Images that ship them still work; operators use one of the other
rows. Avoid `-f`: it skips the teardown, so the host sees an
unexplained reset.

`USR1` and `USR2` are stop signals only in machine mode. In a container
they stay ordinary signals forwarded to an `--init` CMD, because an
operator's `docker kill -s USR1` is meant for the workload (nginx
reopens its logs on it). In a machine nobody outside the guest can send
them, and busybox's meaning for PID 1 is the one that applies.

## The reset line

Immediately before `reboot()`, zpinit writes one line to `console`,
because a guest reset looks the same to the host whether the guest was
done, stopped, or panicked:

```
zpinit: reset intent=shutdown cause=exit status=0
zpinit: reset intent=shutdown cause=exit signal=SIGSEGV
zpinit: reset intent=shutdown cause=stop status=0
zpinit: reset intent=shutdown cause=parked
zpinit: reset intent=shutdown cause=exit signal=SIGKILL oom=1
```

- `cause=exit`: the workload ended on its own (or zpinit failed before
  it started). `cause=stop`: a graceful stop was requested
  (Ctrl+Alt+Del, a stop signal, `zpctl shutdown`). `cause=parked`: a
  parked machine was stopped.
- `status=` is the exit status zpinit would have exited with in a
  container. `signal=` replaces it when the wrap-mode CMD was killed by
  a signal (the container status would be 128+N). This line is the
  only place the status is published.
- `intent` describes how the machine ended, not what should happen
  next. zpinit always writes `shutdown`: a guest cannot restart itself
  in place, so every ending, including an in-guest `reboot`, is a
  shutdown from zpinit's side. Whether the machine runs again is the
  host's restart policy; `cause` and `status`/`signal` are the facts it
  can key on. With `cause=exit`, a non-zero status or a signal means
  the workload failed. With `cause=stop` they describe how the stop
  went: a wrap CMD that dies of its own stop signal reports
  `signal=SIGTERM` (or whichever `stop.signal` is), one killed at the
  deadline `signal=SIGKILL`, and a supervise stop cut off at the
  deadline `status=1`.
- `oom=1` is appended when the process the status belongs to (the
  wrap-mode CMD, or supervise mode's `exit_code_from` target) was
  killed by the kernel's OOM killer: the machine needs more memory, as
  opposed to a `SIGKILL` at the stop deadline. Absent otherwise. See
  [Out-of-memory kills](#out-of-memory-kills).
- `intent=reboot` is reserved; nothing produces it, so host matchers
  should accept it from the start.
- Always its own line: preceded by `\n` so a partial line from the
  workload cannot swallow it, written by opening the console directly
  (so it appears even with logging off), and drained (`tcdrain`) before
  `reboot`. Only a character device is written: a `console` that names
  anything else (a disk, a file, a FIFO) gets no line and an error in
  the log, never an overwrite. The tty turns `\n` into `\r\n`; the host strips `\r`.
  Hosts match lines beginning `zpinit: reset `.
- No line means unknown: a kernel panic, the guest calling `reboot(2)`
  itself, or a Ctrl+Alt+Del before zpinit started.
- **A stable contract.** The prefix `zpinit: reset `, `intent` from
  the set `shutdown` and `reboot` (a host should treat any other value,
  or no line, as an unexplained reset), and no version marker: an
  incompatible change gets a new prefix instead. Fields are only ever
  added (as `oom` was), so a host ignores keys it does not know. The line stays far
  below 1024 bytes, so a host may drop longer lines on purpose; every
  field is bounded and a test enforces it. A host should exempt a
  `cause=parked` reset from any restart policy: the machine never
  started its workload.
- **Forgeable by design.** The guest owns its serial port. Forging
  `shutdown` buys nothing the guest could not get by exiting, so the
  host must never act on this line more strongly than that.

## Out-of-memory kills

Inside a machine the kernel's OOM killer is the only memory limit, and
its victim dies of `SIGKILL`, which on its own looks the same as a kill
at the stop deadline. So in machine mode, when a process zpinit spawned
and tracks dies of `SIGKILL`, zpinit looks for the OOM killer's
`Killed process <pid> (...)` record in the kernel log (`/dev/kmsg`),
counting only records written after that process was spawned, so a
recycled pid cannot match. A match is reported:

- as `oom=1` on the reset line, when it was the wrap-mode CMD or the
  `exit_code_from` target;
- as a warning, `killed by the kernel's OOM killer`, for any service
  (and for the CMD);
- in `zpctl status`: `oom_killed` in `last_exit` with `--json`, and an
  `oom_killed` marker on the `--verbose` line.

Only processes zpinit itself spawned are covered: the CMD, each
service's main process, and exec sessions. A worker forked by a
service (a php-fpm or nginx worker) is its parent's child, so its OOM
kill shows in the kernel log, not in zpinit's reports. In a container
none of this runs: there `/dev/kmsg` is the host's log, with pids from
another namespace.

## Rescue

When bringup fails (no network, a mount failed, the user does not
resolve), the bringup init reports the failure on the console in its
own words, writes `rescue` into `machine.json`, and execs zpinit with
`--skip-entrypoint --config /run/zpinit/rescue.d`. If the root disk
never mounted, this happens inside the initramfs.

With `rescue` set, zpinit ignores the CMD and the config directory
entirely: no entrypoint.d, no services, no CMD, no control socket, no
boot banner. It keeps reaping, keeps the exec listener (when `exec` is
set), and waits. Flags are still parsed first, so an invalid flag ends
the machine with `cause=exit status=2` instead of parking.
A graceful stop ends the machine with `cause=parked`. The machine is
there to be inspected over exec and then destroyed.

## Exec over vsock

The host's exec (the microVM's `docker exec`, and the only way into a
parked machine) is a goroutine in PID 1: accept, resolve the user, set
env and working directory, fork.

- **Transport:** `AF_VSOCK` stream on `exec.port`, bound on every local
  CID. Firecracker's host-side `CONNECT <port>` handshake never reaches
  the guest.
- **Only peer CID 2 (`VMADDR_CID_HOST`) is served**; anything else is
  closed. vsock has no `SO_PEERCRED`, so this is the whole
  authentication; without it a local process could reach a root exec
  over vsock loopback. The host side never listens.
- **Never in a container.** Up to and including Linux 6.18, vsock is not
  network-namespaced, and newer kernels default to the global mode, so
  a container on a host that is itself a VM would open a listener the
  hypervisor can reach. Hence the double gate: machine mode **and** an
  `exec` block.
- A **non-host peer is refused**. Testing that from inside the guest
  needs vsock loopback (`CONFIG_VSOCKETS_LOOPBACK`) in the guest kernel,
  so a local process can connect to the guest's own CID.
- Starts after entrypoint.d has finished (the reaper is not running
  before that); a hung entrypoint.d script is bounded by its
  `entrypoint_script_timeout`. Closes when the graceful stop begins
  (supervise mode) or the CMD exits (wrap mode): new execs are refused
  during teardown.

### Protocol v1

One exec per connection; connections are concurrent. The codec is the
public Go package
[`github.com/0ploy/zpinit/execproto`](../execproto/proto.go)
(standard library only): host-side clients import it rather than re-implementing the framing. Its frame
values and limits change only additively; an incompatible change bumps
`Version`.

1. The host sends the request: a 4-byte big-endian length, then that
   many bytes of JSON (at most 1 MiB, unknown fields rejected):

   ```json
   {"v": 1, "argv": ["sh", "-l"], "user": "app", "working_dir": "/srv",
    "env": ["DEBUG=1"], "tty": true, "rows": 40, "cols": 120}
   ```

   `user`, `working_dir`, `env`, `rows` and `cols` are optional. A
   request that cannot be decoded gets an `error` frame within 10 s.
2. zpinit replies with exactly one `started` or `error` frame.
3. Both sides then exchange frames: 1-byte type, 4-byte big-endian
   payload length (at most 1 MiB), payload.

| Type | Byte | Direction | Payload |
| --- | --- | --- | --- |
| `started` | 1 | to host | `{"pid": n}` |
| `stdin` | 2 | to guest | bytes |
| `stdin_eof` | 3 | to guest | empty. Closes stdin; with a tty, sends the terminal's EOF (`^D`) |
| `resize` | 4 | to guest | `{"rows": r, "cols": c}`, tty only |
| `signal` | 5 | to guest | a signal name (`SIGINT`, or `SIG<n>`), sent to the exec's process group |
| `stdout` | 6 | to host | bytes; with a tty, the merged terminal stream |
| `stderr` | 7 | to host | bytes; without a tty only |
| `exit` | 8 | to host | `{"status": n}` or `{"signal": "SIGKILL"}`, then close |
| `error` | 9 | to host | a message, then close |

Semantics:

- **User:** the request's `user`, else `exec.user`. Names or numeric
  ids, `user[:group]`; supplementary groups from `/etc/group`. A numeric
  uid with no passwd entry runs with gid 0, as under container
  runtimes.
- **Env:** `exec.env`, then the request's `env` on top (`docker exec`
  semantics). Nothing else: not `[env]` from `zpinit.toml`, not what
  entrypoint.d added. `HOME` follows the exec user when the request
  picked a user other than the default, and is set to the user's home
  (else `/`) when absent; `TERM=xterm` is defaulted for a tty; `PATH`
  defaults to `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`.
  `argv[0]` is resolved against that `PATH`.
- **Working directory:** the request's, else `exec.working_dir`, else
  `/`.
- Each exec is `setsid`'d, so it is its own session and process group.
  With a tty it gets a fresh pty from `/dev/ptmx` as its controlling
  terminal.
- **The exec's session does not outlive the exec.** When the
  connection drops before the exit, every process in the exec's
  session is `SIGKILL`ed, including jobs a tty shell moved into process
  groups of their own; when the exec's main process exits, whatever is
  left of its session is `SIGKILL`ed too, and output still buffered is
  delivered (for up to 2 s) before the `exit` frame. A process that
  escaped the session with its own `setsid` survives until the machine
  ends. A `signal` frame, by contrast, goes to the exec's process group
  only, as a signal sent to the command would.
- **Stdin has backpressure.** zpinit queues a bounded amount of stdin
  and stops reading the connection while the exec is not taking it, so
  a fast host slows down instead of growing memory in PID 1. Frames
  behind that stdin (`resize`, `signal`) wait with it. A dropped
  connection is still noticed while stalled: zpinit checks for a hangup
  every 250 ms.
- **The listener survives running out of fds or memory.** `accept`
  failing with `EMFILE`, `ENFILE`, `ENOBUFS` or `ENOMEM` is retried with
  backoff (up to 1 s) instead of closing the listener.
- Exec sessions are killed by the teardown's `kill(-1)`.

## Testing

- Unit: the detection predicate, the strict `machine.json` decoder, the
  reset-line format, mount settling order, the frame codec, and full
  exec sessions (pipes, tty, signals, connection drop) over a socket
  pair.
- Container integration (`test/integration/init_test.go`): as a
  container's PID 1 with a planted `machine.json`, zpinit reports
  `context=container`, ignores the file, opens no listener, reaps
  orphans under `--init`, and exits with the CMD's status; stray
  signals do not kill it.
- Machine mode itself needs a real kernel and is covered from the host
  by a microVM boot harness judged on the serial log: Ctrl+Alt+Del ->
  `cause=stop`; the workload exiting -> `cause=exit status=N` with no
  `Attempted to kill init`; writes before the exit survive; park ->
  `cause=parked`. Still to run there: exec into a parked machine, and
  an exec from a non-host CID being refused.
