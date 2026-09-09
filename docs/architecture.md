# Architecture

A single static Go binary, CGO disabled, built with `-trimpath`. ~3 MB.
Linux-only in production (uses `Pdeathsig`, `Setpgid`, `/proc`); macOS
dev compiles via build tags but doesn't exercise PID-1 paths.

## Packages

| Package                | Role                                                                                           |
| ---------------------- | ---------------------------------------------------------------------------------------------- |
| `cmd/zpinit`           | Supervisor binary. Mode detection, signal loop, dispatch.                                      |
| `cmd/zpctl`            | Thin control client.                                                                           |
| `internal/config`      | TOML loading, defaults, validation, `--check-config`.                                          |
| `internal/entrypoint`  | `entrypoint.d/` runner, env-file propagation.                                                  |
| `internal/reaper`      | Centralized `wait4(-1, WNOHANG)` loop with PID dispatch.                                       |
| `internal/service`     | Process spawn with SysProcAttr, credentials, log destinations.                                 |
| `internal/supervisor`  | Per-service state machine, orchestrator (boot, readiness, reload, shutdown), control server.  |
| `internal/ctlproto`    | Wire protocol between zpinit and zpctl.                                                        |
| `internal/resources`   | cgroup v1/v2 + /proc CPU/memory detection. Produces the `ZPINIT_CPU_*`/`ZPINIT_MEMORY_BYTES` env vars. |

## Per-service state machine

```
pending → starting → running → stopping → stopped
                 ↘ backoff ↗
                 ↘ fatal
```

Backoff doubles from `backoff_initial` to `backoff_max`, resets after
the service stays up for `backoff_reset_after`, and gives up after 5
consecutive crashes (FATAL). The retry budget is hardcoded
(`MaxConsecutiveCrashes = 5`).

## Boot sequence

1. **entrypoint.d/** runs serially in filename order, each with
   `entrypoint_script_timeout` applied. A non-zero exit is fatal
   unless `entrypoint_on_failure = "continue"`. Scripts can append
   `key=value` to `/run/zpinit/env` to propagate env to services.
2. **Mode detection.** If `flag.Args()` is non-empty after zpinit
   parses its own flags, a CMD was provided: zpinit `syscall.Exec`s
   it as PID 1 and ignores `services/`.
3. **Resource detection.** `internal/resources` resolves this
   process's own cgroup directory from `/proc/self/cgroup` +
   `/proc/self/mountinfo` (the cgroupfs mount root is only the
   container's cgroup under `--cgroupns=private`), then reads cgroup
   v2 / v1 and `/proc`, taking the min of all sources. A failed
   resolution sets `Snapshot.CgroupResolved = false`, which warns at
   boot and is a `--doctor` finding: the numbers may be the host's. Reservations from
   `[resources]` are subtracted; the result is exported as
   `ZPINIT_CPU_COUNT`, `ZPINIT_CPU_QUOTA`, and `ZPINIT_MEMORY_BYTES`
   at the top of the env precedence chain so neither container env
   nor entrypoint scripts can shadow the detected values. Validation
   rejects the keys in any operator `[env]` table. When some service
   needs it (see Live resource watcher), a watcher keeps polling after
   boot and commits debounced deltas (env updates,
   `reload_on_change` fanout, `replicas = "auto"` rebalancing);
   otherwise the detected values are fixed for the container's life.
   The `[resources]` reservation values themselves are read once at
   boot; changing them requires a container restart.
4. **Service boot.** In supervise mode, services start in filename
   order. Each readiness probe blocks the next service's start.
   `boot_timeout` is a per-service budget: each service gets its own
   window, so a legitimately slow first service cannot starve later
   services of their probe time. A service that does not come up
   inside its window (or dies before it is ready) fails boot, which
   tears everything down and exits 1, so an orchestrator sees a
   container that could not start. Per-service
   `on_boot_failure = "continue"` opts out: the failure is logged, the
   service is left in whatever state it reached and stays visible in
   `zpctl status`, later services still boot, and PID 1 keeps running.
   Only initial boot consults it; a reload-added service that fails to
   boot has never been able to abort the container.

## Reload

`SIGHUP` (or `zpctl update`) re-reads `/etc/zpinit/` and diffs against
the running set. Added or restart-flagged runners are registered
synchronously; their boots run in a single detached goroutine, one at
a time in filename order, so readiness still blocks the next start.
The boot goroutine uses `runnerCtx`, not the reload caller's context,
so it survives client disconnect.

Back-to-back reloads serialize their boot phases on `reloadBootMu`:
reload N+1's adds wait for reload N's adds to finish booting before
they start. Without this, two reloads landing seconds apart could
boot their adds concurrently and break the "later filename does not
start until earlier filename is ready" invariant that initial boot
relies on.

`exit_code_from` is rebound on every reload, so the watched service
can be added, removed, or retargeted. Each installation carries a
generation counter; a watcher goroutine re-checks its generation before
firing, because cancelling the old watcher does not synchronize with
its progress. See Shutdown for what the watcher fires on.

Each service file is parsed and validated independently (see
`docs/configuration.md`). A file that fails to parse or validate is
skipped, recorded in `Config.SkippedFiles`, and excluded from the
diff's remove set: a service whose file just became unparseable is
left running with its last-good config rather than being torn down
over a typo. The valid files reload normally. `zpctl` exits non-zero
when any file was skipped; daemon boot and SIGHUP log and continue.

### Live resource watcher

The watcher is **gated**: `runSupervise` starts it only when
`Config.NeedsResourceWatch()` is true, i.e. some service declares
`replicas = "auto"` or a non-empty `reload_on_change`. Those are the
only two consumers of a committed `Change`, so with neither present
the loop would poll for nobody — and with a few hundred containers on
one host that idle poll is the largest thing zpinit does. When the
watcher is off, `ZPINIT_CPU_COUNT` / `ZPINIT_CPU_QUOTA` /
`ZPINIT_MEMORY_BYTES` stay at their boot-time detected values.
`Orchestrator.SetConfigCommitHook` re-evaluates the predicate after
every committed reload so a newly-added auto / `reload_on_change`
service still arms it (`Watcher.Start` is idempotent and reports
whether it was the call that started polling). The transition is
one-way: removing the last such service leaves the watcher running.

When started, the watcher's trigger is **inotify, not polling**. It
arms `IN_MODIFY` watches on the cgroup limit files of the RESOLVED
cgroup directory (`cgroupLimitPaths`: the v2 and v1 paths together,
whichever exist) and parks on a blocking read. Every runtime-driven quota change is a
userspace write to one of those files — `docker update`, a Kubernetes
in-place pod resize, systemd — and the kernel raises `IN_MODIFY`
through the generic `vfs_write` path, so the event arrives in
milliseconds and an idle container costs zero wakeups. Only
`IN_MODIFY` is requested: `IN_ACCESS`/`IN_OPEN` would fire on the
`Detect` a wake triggers, making the loop self-sustaining.

An event carries no usable payload. The runtime writes `cpu.max` and
`memory.max` as separate, non-atomic writes, so a wake can land on a
half-applied change; the wake means only "re-`Detect`", and the
debounce below settles it. Watches are re-armed on every observation
rather than tracked — `inotify_add_watch` is idempotent, returning the
same descriptor — which self-heals an `IN_IGNORED` (the inode went away
under a cgroupfs remount, silently dropping the watch) and a transient
`ENOSPC` (another process in the container exhausting the per-UID watch
budget).

Wakes are rate-limited to one observation per `observeCooldown`
(100 ms). Because wakes are demand-driven, a process with write access
to cgroupfs (cgroup delegation: nested containers, systemd-in-container)
could otherwise drive a full `Detect` per write. A wake inside the
window is deferred to the end of it, never dropped, so batching costs
latency and never correctness.

A backstop ticker (`[resources] poll_interval`, default 15 min) re-runs
`Detect` on its own. It is not the primary path; it covers only what
inotify cannot see: a limit the kernel derives internally rather than
by a write to our file (a parent cgroup's cpuset propagating into
`cpuset.cpus.effective`), and a sandbox runtime that accepts the watch
but never delivers, which is silent and undetectable. Off Linux there
is no notifier at all and the backstop carries the whole job.

Both triggers run the same observation, and the debounce below is
unchanged from the polling design. When the exposed integer
(`ZPINIT_CPU_COUNT`) or uint64 (`ZPINIT_MEMORY_BYTES`) value differs
from the last committed Snapshot, a per-direction debounce timer starts
(`scale_down_after` when any dimension moves down, including
mixed-direction moves; `scale_up_after` only when everything moves
up). If the new value still holds when the timer fires, the
watcher commits and emits a `Change` carrying the new Snapshot and
the list of dimensions (`"cpu"`, `"memory"`) that moved. A
transient flip that returns to baseline within the debounce window
emits nothing.

The orchestrator's `OnResourceChange` consumes the channel:
it updates the internal `resourceEnv` (so the next reload-driven
recompose of baseEnv sees the new values), recomposes baseEnv via
the installed builder so freshly-spawned children pick up the new
env, then fans out a reload action to every runner whose
`reload_on_change` list intersects the changed dimensions.
Sub-integer quota wobble that doesn't move the floor is invisible
by construction.

Together the gate and the inotify trigger make an idle container's
resource tracking free: containers with no `replicas = "auto"` /
`reload_on_change` service run no watcher at all, and the ones that do
sleep in the kernel until a quota actually moves. The remaining known
cost is `Detect` itself, which re-reads `/proc/cpuinfo` in full on
every observation — cheap now that observations are rare, but the
obvious next optimization is to cache the `/proc` half (it cannot
change for a running container) and re-read only the cgroup files.

### Per-service reload action

`zpctl reload <name>` and the watcher-driven `OnResourceChange`
trigger both run through `Orchestrator.ReloadService`, which
dispatches per runner:

- `reload_signal` set → `SignalGroup`. In-place; the running process
  re-reads its config (or whatever it's wired to do on the signal).
- `reload_command` set → one-shot spawned via the centralized
  reaper. Inherits the service's env so it sees `ZPINIT_CPU_COUNT`
  and friends. Capped at 30 s before we stop waiting on it. Non-zero
  exit codes are surfaced as `zpctl reload` errors with the
  "service still running" qualifier, so CI pipelines that chain
  `zpctl reload && next` fail closed on a broken reload payload
  without restarting the supervised process itself.
- Neither → full stop+start (same as `zpctl restart`).

Parallelism mirrors `stopRunnerGroup`: parallel within a replica
group, serial between filename groups. `zpctl reload` with no
arguments stays a backwards-compatible alias for `update`
(config reread + apply).

## Shutdown

Three things end a supervise-mode container: a signal, `zpctl
shutdown`, and the `exit_code_from` watcher. Nothing else does, once
boot has completed. A service crashing, crash-looping to FATAL, or
being stopped by an operator leaves PID 1 running, which is what keeps
`docker exec` and `zpctl` usable on a container whose app is broken.
(Initial boot is the exception, and it happens before this section
applies: see Boot sequence step 4.)

`exit_code_from = "<service>"` opts one service into owning the
container's lifetime. The watcher fires only when that service reaches
a terminal state *on its own*: an exit under a policy that does not
restart it, or FATAL after the crash budget runs out. Reaching Stopped
because an operator stopped it does not count, and `RestartCtx`
transits Stopped between its stop and start halves, so treating any
terminal edge as final used to make `zpctl restart <target>` shut the
container down seconds after the service had come back up. Each
terminal transition latches why it happened (`Runner.LastTerminal`,
written before the state change so an observer woken by the transition
already sees it); the watcher reads the latch, and on an operator edge
parks in `WaitLeftTerminal` and resumes watching rather than returning,
so a service restarted once can still end the container later. Firing
closes `earlyShutdownCh`, which `Run` selects on alongside `ctx.Done`,
and the container exits with the service's code (`128 + signal` if it
was signaled).

`SIGTERM`, `SIGINT`, or `SIGQUIT` to PID 1 triggers `stopAll`
(`SIGQUIT` for supervisord parity: operators use `kill -QUIT` there
for graceful shutdown). `SIGUSR1`/`SIGUSR2` are discarded rather than
left at the Go default of killing the process; `SIGHUP` during the
entrypoint phase is discarded too (reload only makes sense once
services are supervised). Once shutdown begins, mutating control
verbs (`start`, `restart`, `update`, reloads, autoscale commits) are
refused and the control socket stops accepting connections, so no
service can be spawned into a teardown that would never stop it
gracefully. Services are
teardown'd by filename group, in reverse filename order. Between
groups the teardown is sequential: filename order encodes dependency
order during boot, so reverse-serial between groups lets dependents
drain through their dependencies before the dependency itself
receives `SIGTERM`. WITHIN a group (all replicas of one filename),
replicas are signaled and awaited in parallel: they are the same
logical service and have no inter-replica flush ordering, so
serializing them would multiply teardown time by N for no semantic
gain. Per-runner `SIGKILL` escalation (handleStopKillTimeout) bounds
any stuck replica.

The outer wait budget is recomputed at signal time (it can't be
snapshotted at boot, because reload can change service count and
`stop_timeout` after launch). The budget counts one `(stop_timeout +
reapGrace)` per filename group, not per runner, matching stopAll's
parallel-within-group schedule. The supervisor outer wait must always
cover stopAll's inner wait, otherwise the runtime hard-kills PID 1
mid-graceful-shutdown.

The same parallel-within-group / serial-between-groups schedule
applies to `Reload`'s remove and restart-stop paths via
`removeServiceGroup`, including the reverse filename order between
groups: deleting `10_redis.toml` and `20_php.toml` in one reload
drains php through redis before redis is signaled, exactly like
shutdown. Without the group parallelism, `replicas = 64` with the
default 10s stop_timeout would burn ~16 minutes per logical service
on stuck children during a reload.

A second `SIGTERM`/`SIGINT`/`SIGQUIT` arriving while the shutdown
wait is already running is deliberately ignored: graceful stop is
already proceeding at full speed and per-runner `SIGKILL` escalation
is the accelerator. For an immediate hard kill, use the runtime's
(`docker stop -t 0`); Pdeathsig takes the children down with PID 1.

## Reaping

One `wait4(-1, WNOHANG)` site, in `internal/reaper.Reap`, dispatched
by PID to per-service exit channels. Never `cmd.Wait()` per service:
the two race against each other; whichever the kernel satisfies first
wins, the loser gets `ECHILD`, the exit code is lost. tini does it the
same way.

`SpawnTracked` holds its mutex across `cmd.Start()` so the new PID is
registered atomically, closing the spawn-then-track race for
fast-dying children.

## Control protocol

`zpctl` talks to zpinit over a Unix socket (default
`/run/zpinit.sock`) with a line-based plaintext format. Each request
is one line, each response is a status line plus zero or more body
lines, terminated by `.` on its own line. Operators can debug live
with `nc` or `socat`.

State names match supervisorctl exactly (`RUNNING`, `STOPPED`,
`BACKOFF`, `FATAL`, ...) so existing muscle memory transfers.

Target syntax transfers too. The daemon resolves every verb's service
argument through `resolveTarget`, which only understands the native
forms `NAME` (all replicas) and `NAME/N` (one replica). supervisord's
`group:process` targets are translated to those forms **client-side in
`zpctl`** (`translateSupervisorTarget`), before the request leaves the
client: `NAME:*` and `NAME:NAME` map to all replicas, `NAME:NAME_N` (the
default `%(program_name)s_%(process_num)0Nd` naming) maps to `NAME/N`.
Service names are constrained to `[a-zA-Z0-9_-]+`, so a `:` is always
supervisord syntax and never collides with a real name; an unrecognized
process suffix is rejected client-side rather than silently widening to
the whole group. Keeping the shim in the client (not the daemon) means
it works against any daemon version: PID 1 cannot be hot-swapped, so a
container running an older `zpinit` still honors `worker:*` from an
updated `zpctl`.

The response status-line `code` maps 1:1 to `zpctl`'s process exit
status, and the taxonomy is stable for machine consumers: `0` success,
`1` operation failed, `2` daemon unreachable (set by `zpctl` itself on
a connect or mid-request IO error; the daemon never returns it), `3`
unknown service. The daemon wraps "no such service" resolution errors
so handlers map them to `3` (`errRespFor`); a consumer treats that as
"stopped/absent" rather than a hard failure.

Machine-readable output rides the same line protocol: `status --json`
emits one compact JSON object per body line (NDJSON), never
pretty-printed multi-line JSON, because every body line is run through
`sanitizeLine`. `resolve` returns a single JSON line the same way.

`start --wait` / `restart --wait` are non-streaming but can run for a
service's `boot_timeout` plus its `[ready].timeout`; the handler's
dispatch budget is extended to cover that and the client skips its
fixed 30s read deadline for them (as it does for streaming verbs).
`update NAME...` applies only the named services' add/remove/restart
from the full diff and never commits a global `[env]` change (that
would restart every reloadable service); a later argument-less
`update` still applies the deferred global change. It shares Reload's
apply path (`applyReloadDiff`); only the diff is filtered and the
committed config keeps the running globals.

A small protocol extension supports **streaming** responses for
`tail --follow`: the server writes the status line and terminator
the same way, but emits body lines as new bytes arrive on the
watched log file and only writes the terminator when the stream
ends (client disconnect, supervisor shutdown). The connection's
read deadline is cleared for the streaming verb's lifetime; the
write deadline is refreshed per drain so a wedged client is still
bounded. `zpctl` detects `--follow` (or `-f`) and switches its
client-side read loop to read body lines as they arrive rather
than buffering until the terminator.

Body lines are complete log lines: a writer caught mid-line is held
until its newline arrives instead of being delivered as two frames.
A single line longer than 32KiB is split into chunks below the
64KiB wire cap rather than aborting the client with a protocol
error. The follow loop detects rotation by inode change (logrotate's
default rename mode) and copytruncate rotation by the file shrinking
below the consumed offset; either way it keeps following the path
rather than the dead offset.

### Access control

Two layered gates:

1. **Filesystem.** Umask is tightened to `0o077` across the bind so
   the socket is born `0700`; an explicit `chmod 0600` follows as
   belt-and-braces. Without the umask flip, `bind(2)` creates the
   socket as `0777 & ~umask` (typically `0755`) for the few
   microseconds before chmod — long enough for a non-root local
   process to `connect()` and keep the FD past chmod.
2. **Peer credentials.** Every accepted connection is gated by
   `SO_PEERCRED`: peer UID must equal the daemon's effective UID.
   Connections from any other UID are rejected without dispatch and
   logged with peer PID. Linux-only; the macOS dev build skips this
   check.

Net effect in a typical container (PID 1 = root): only root can use
zpctl. A future move to allow non-root operators would lift the
`SO_PEERCRED` check rather than loosen the filesystem permissions.

Response framing escapes CR/LF and lone `.` body lines via
`ctlproto.sanitizeLine`, so a tainted log line surfaced by
`zpctl tail` (or a multi-line TOML parse error from `zpctl update`)
can't end the body early or split a single field across lines.

## Replicas: no cluster harness

`replicas = N` on a service produces N first-class supervised Runners
from one TOML file. The orchestrator's diff/reload layer keeps
filename as the identity key (one TOML file = one logical service),
but the in-memory runner set is the expansion: every replica has its
own PID, log file, crash budget, and zpctl row.

We deliberately did **not** ship a Node-side cluster harness (a daemon
that forks N workers behind one listening socket). Modern Node (>=
22.12.0), Bun (any 1.x), and Deno (any modern) expose `reusePort: true`
on `listen()` natively; the kernel maintains a single `(addr, port)`
group with N sockets and dispatches incoming SYNs by 4-tuple hash. No
master process, no IPC, no shared listener. Each replica is
independent. zpinit just spawns N copies; the runtime handles port
sharing.

This dropped a lot of complexity (no master goroutine, no FD passing,
no per-runtime cluster shim) at the cost of `cluster.worker.id` /
`process.send` IPC, which apps that hard-depend on Node's `cluster`
module would need to refactor away from. For the workloads zpinit
targets (PHP-CLI consumers, Sidekiq-style workers, plain HTTP servers
with no cross-worker IPC) the trade is one-sided.

`zpinit --doctor` covers the listener-floor case: it detects when any
service declares `replicas > 1` while the node binary on PATH is below
22.12.0 and emits a WARN naming the EADDRINUSE failure mode.
