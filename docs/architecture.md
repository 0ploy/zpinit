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
3. **Resource detection.** `internal/resources` reads cgroup v2 / v1
   and `/proc` (taking the min of all sources). Reservations from
   `[resources]` are subtracted; the result is exported as
   `ZPINIT_CPU_COUNT`, `ZPINIT_CPU_QUOTA`, and `ZPINIT_MEMORY_BYTES`
   at the top of the env precedence chain so neither container env
   nor entrypoint scripts can shadow the detected values. Validation
   rejects the keys in any operator `[env]` table. Detection is
   one-shot at boot; live updates land in a later release.
4. **Service boot.** In supervise mode, services start in filename
   order. Each readiness probe blocks the next service's start. The
   `boot_timeout` budget covers the whole phase, starting when this
   step begins (not at zpinit launch).

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
can be added, removed, or retargeted.

### Live resource watcher

A polling goroutine in `internal/resources.Watcher` re-runs
`Detect` once a second. When the exposed integer (`ZPINIT_CPU_COUNT`)
or uint64 (`ZPINIT_MEMORY_BYTES`) value differs from the last
committed Snapshot, a per-direction debounce timer starts
(`scale_up_after` for any upward move, `scale_down_after`
otherwise). If the new value still holds when the timer fires, the
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

Polling is intentionally simple; inotify on cgroupfs would cut the
median detection latency but is left as an optimization since one
file-read per second is essentially free.

### Per-service reload action

`zpctl reload <name>` and the watcher-driven `OnResourceChange`
trigger both run through `Orchestrator.ReloadService`, which
dispatches per runner:

- `reload_signal` set → `SignalGroup`. In-place; the running process
  re-reads its config (or whatever it's wired to do on the signal).
- `reload_command` set → one-shot spawned via the centralized
  reaper. Inherits the service's env so it sees `ZPINIT_CPU_COUNT`
  and friends. Capped at 30 s before we stop waiting on it; non-zero
  exit is logged, not surfaced as an error from `zpctl`.
- Neither → full stop+start (same as `zpctl restart`).

Parallelism mirrors `stopRunnerGroup`: parallel within a replica
group, serial between filename groups. `zpctl reload` with no
arguments stays a backwards-compatible alias for `update`
(config reread + apply).

## Shutdown

`SIGTERM` or `SIGINT` to PID 1 triggers `stopAll`. Services are
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
`removeServiceGroup`. Without that, `replicas = 64` with the default
10s stop_timeout would burn ~16 minutes per logical service on stuck
children during a reload.

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
