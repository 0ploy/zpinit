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
3. **Service boot.** In supervise mode, services start in filename
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

`exit_code_from` is rebound on every reload, so the watched service
can be added, removed, or retargeted.

## Shutdown

`SIGTERM` or `SIGINT` to PID 1 triggers `stopAll`: services are
signaled and waited one at a time, in reverse filename order.
Filename order encodes dependency order during boot, so reverse-serial
teardown lets dependents drain through their dependencies before the
dependency itself receives `SIGTERM`. Per-service `SIGKILL` escalation
bounds any one stuck service.

The outer wait budget is recomputed at signal time (it can't be
snapshotted at boot, because reload can change service count and
`stop_timeout` after launch). The supervisor outer wait must always
cover stopAll's serial inner wait, otherwise the runtime hard-kills
PID 1 mid-graceful-shutdown.

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
lines, terminated by a blank line. Operators can debug live with `nc`
or `socat`.

State names match supervisorctl exactly (`RUNNING`, `STOPPED`,
`BACKOFF`, `FATAL`, ...) so existing muscle memory transfers.
