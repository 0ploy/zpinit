# Troubleshooting

Symptom-indexed runbook for debugging a live container. Each entry is
what you see, why it happens, and what to do about it.

Start here for anything that reproduces at boot:

```sh
zpinit --doctor          # binaries, runtimes, cgroup detection, live state
zpinit --plan            # the resolved boot plan, without running it
zpctl status --verbose   # per-service state, RSS, CPU, fds, spawn count
```

`--doctor` needs no running instance, so it works in a `docker run
--rm image zpinit --doctor` throwaway as well as via `docker exec`.

---

## The container reports far more CPU or memory than it was given

**Symptom.** `ZPINIT_CPU_COUNT` is the host's core count, an app sizes
its worker pool or heap for a machine it does not have, or a
`replicas = "auto"` service starts many more replicas than the CPU
limit should allow. The boot log carries:

```
level=WARN msg="could not locate this container's own cgroup; CPU/memory figures are UNVERIFIED and may describe the host"
```

**Why.** zpinit resolves its own cgroup from `/proc/self/cgroup` plus
`/proc/self/mountinfo`. The cgroupfs mount root is only the container's
cgroup under `--cgroupns=private`; under `--cgroupns=host` (the default
on cgroup v1 hosts), with the host's `/sys/fs/cgroup` bind-mounted in,
or under some Kubernetes/CRI and LXC layouts it is the host's root
cgroup, which holds no limits. If resolution fails, detection falls back
to `/proc`, which sees the whole machine.

**Check.**

```sh
zpinit --doctor | grep -i cgroup
cat /proc/self/cgroup
grep -E ' cgroup2? ' /proc/self/mountinfo
```

A healthy cgroup v2 container shows `0::/` and a `cgroup2` mount at
`/sys/fs/cgroup`. Seeing `0::/docker/<id>` is fine too, as long as
`--doctor` says the cgroup resolved.

**Fix.** Make the container's cgroup reachable: prefer
`--cgroupns=private`, and don't bind-mount the host's `/sys/fs/cgroup`
over the runtime's own mount. If neither is possible, treat the figures
as unverified — pin `replicas` to a static number instead of `"auto"`,
and set any heap or worker-pool size explicitly rather than deriving it
from `ZPINIT_CPU_COUNT`.

## `ZPINIT_CPU_COUNT` never changes after a quota update

**Symptom.** `docker update --cpus` (or a Kubernetes in-place resize)
lands, but a service that respawns afterwards still sees the boot-time
value.

**Why.** Expected, and visible in the boot log:

```
level=INFO msg="resource watcher not started; no service uses replicas=\"auto\" or reload_on_change"
```

The live resource watcher only runs when something consumes its output.
Polling in every container costs a wakeup per second on the shared host
to track a value that changes about never, so with no `replicas =
"auto"` and no `reload_on_change` service, zpinit detects once at boot
and stops there.

**Fix.** Opt the service in:

```toml
reload_on_change = ["cpu", "memory"]
```

The boot log then shows `watching cgroup limit files for changes`. See
[configuration.md](configuration.md#resources).

## A service crash-loops to FATAL with EADDRINUSE

**Symptom.** `zpctl status` shows `FATAL`; the log shows every replica
past the first failing to bind.

**Why.** `replicas = N` runs N independent processes. Unless the app
opts into `SO_REUSEPORT`, only the first to bind wins.

**Fix.** `zpinit --doctor` catches the common Node case (needs
≥ 22.12.0 and `server.listen({ reusePort: true })`). For other
runtimes, either enable the equivalent socket option or drop to
`replicas = 1` and scale with more containers. Full detail in
[clustering.md](clustering.md).

## Boot hangs on one service

**Symptom.** Boot stops progressing; later services never start.

**Why.** Filename order is a start order, and each service's `[ready]`
probe gates the next one. A probe that never passes blocks the queue
until `boot_timeout` (default 60s) expires.

**Check.** `zpinit --plan` prints each service's probe and timeouts.

**Fix.** Three independent knobs, in increasing order of tolerance:

- `[ready].timeout` — how long the probe may keep failing.
- `[ready].on_timeout = "continue"` — treat a timed-out probe as ready
  and carry on to the next service.
- `on_boot_failure = "continue"` — keep PID 1 alive even if this
  service never boots, so `docker exec` still works on a broken image.

`on_boot_failure` defaults to `"fail"` on purpose: an orchestrator needs
a container that cannot start to say so. Only relax it for dev images.

## The whole container exits right after a reload

**Symptom.** A `zpctl update` or SIGHUP is followed by clean shutdown.

**Why.** The service named by `exit_code_from` was removed by that
reload, or reached a terminal state on its own. That key means "this
container exists to run that service", so the container follows it down.

**Not** the cause: `zpctl stop`/`restart` on the watched service.
Operator-driven terminal states are explicitly excluded.

**Fix.** Keep the watched service's file in place, or point
`exit_code_from` elsewhere before removing it.

## A reload appears to ignore a file

**Symptom.** You edited a service file, ran `zpctl update`, and nothing
changed.

**Why.** Most likely the file failed to parse or validate and was
skipped. One bad file never aborts the batch, and a running service
whose file is now broken is deliberately left running rather than torn
down over a typo.

**Check.** The skip is reported, with the exact error, by all of:

```sh
zpctl update            # "! 20_worker.toml: skipped (...)" plus a non-zero exit
zpctl reread            # dry-run diff, same reporting
zpinit --check-config /etc/zpinit
```

The daemon also logs it at boot and on SIGHUP.

Two other possibilities: the service has `reloadable = false` (logged as
`config changed but reloadable=false; ignoring`), or the filename starts
with `.` or ends in `.disabled`, both of which the loader skips
silently.

## A service will not die on stop

**Symptom.** `zpctl stop` hangs; the log shows
`stop_timeout exceeded; escalating to SIGKILL` and then
`service did not terminate even after SIGKILL escalation`.

**Why.** A process in uninterruptible kernel sleep (D state) cannot
receive SIGKILL until its syscall returns — typically a wedged NFS or
network mount. zpinit waits a bounded extra window and then abandons
the wait rather than blocking shutdown forever.

**Check.** `cat /proc/<pid>/stat | awk '{print $3}'` — `D` confirms it.
`cat /proc/<pid>/stack` on the host shows where.

**Fix.** Nothing inside the container can force it; resolve the blocked
I/O on the host. zpinit stays responsive meanwhile, and the child dies
with PID 1 via `Pdeathsig` when the container exits.

## `zpctl` says connection refused or permission denied

**Symptom.** `zpctl: connect /run/zpinit.sock: ...` and exit code 2.

**Why and fix**, in order of likelihood:

- **zpinit isn't in supervise mode.** A container started with a CMD
  runs in wrap mode and has no control socket at all. `zpctl` only
  works when zpinit is supervising.
- **Wrong path.** Resolution order is `--socket PATH`, then
  `$ZPINIT_SOCKET`, then `/run/zpinit.sock`. Check
  `control_socket` in `zpinit.toml`.
- **Wrong UID.** The socket is `0600` and every connection is
  additionally gated on `SO_PEERCRED`: the peer's UID must equal the
  daemon's. `docker exec -u root` if the daemon runs as root.
- **Still booting.** The socket comes up before services finish
  booting, but very early requests that add services are refused with
  `supervisor is not running yet`. Retry.

## `zpctl status` shows EXITED, not STOPPED

Both are terminal, and the difference is the cause. `STOPPED` means an
operator stopped it (`zpctl stop`, or the stop half of a restart).
`EXITED` means the process ended on its own under a policy that does not
restart it — normal for `restart = "never"` one-off workers. `FATAL`
means the crash budget was exhausted.

## A replicated service logs everything into one file

**Symptom.** `replicas = 4`, one log file, interleaved output.

**Why.** Intended default: replicas share the log path unless you ask
otherwise. Linux `O_APPEND` is atomic below `PIPE_BUF` (typically 4096
bytes), so line-sized writes do not tear.

**Fix.** Put `{index}` in the path for per-replica files:

```toml
[log]
stdout = "/var/log/worker-{index}.log"
```

`zpinit --doctor` prints the expansion so you can confirm before boot.

## Logs are missing entirely

`log.stdout` / `log.stderr` default to `inherit`, meaning the service
writes to the container's stdout/stderr and shows up in `docker logs`.
If you set a file path instead, that output leaves `docker logs` and
goes only to the file — `zpctl tail NAME` reads it back.

zpinit creates the parent directory, but refuses a symlink at the leaf
(`O_NOFOLLOW`) and anything that is not a regular file. Both surface as
a spawn error naming the path.

There is no log rotation, by design. Use logrotate, or log to
stdout and let the host collect.

---

## Reporting a problem

Include `zpinit --version`, `zpinit --doctor` output, the boot banner
line, and `zpctl status --verbose`. Between them they pin down the mode,
the resolved resource budget, and every service's state.
