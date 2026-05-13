# Changelog

## Unreleased

### Features

- **`zpctl reload <service>` performs in-place reload.** A new
  per-service `reload_signal` (e.g. `"HUP"`) sends the configured
  signal to the running process group; `reload_command` (e.g.
  `["/usr/sbin/nginx", "-s", "reload"]`) runs a one-shot that talks
  to the live process via its own IPC. Neither set means `zpctl
  reload` falls back to a stop+start cycle, so operators get one
  verb that does the right thing per service. Mutually exclusive;
  `--check-config` rejects both at once. `zpctl reload` with no
  arguments stays a backwards-compatible alias for `update` (config
  reread + apply).

- **Detected CPU and memory budget now exposed to every service.**
  At boot, zpinit reads cgroup v2 / v1 and `/proc` (taking the min
  of all sources) and injects `ZPINIT_CPU_COUNT`, `ZPINIT_CPU_QUOTA`,
  and `ZPINIT_MEMORY_BYTES` into the wrapped CMD's env or every
  supervised service's env. nginx wrappers can map onto
  `worker_processes`, the JVM onto `-Xmx`, a clustering shim onto
  fork count. The optional `[resources]` block in `zpinit.toml`
  subtracts a reservation (`reserve_cpu`, `reserve_memory`) before
  children see the numbers, so master processes or sidecars keep
  their headroom. Detection is one-shot at boot for this release;
  live updates land in a follow-up.

### Security

- **`ZPINIT_CPU_COUNT`, `ZPINIT_CPU_QUOTA`, and `ZPINIT_MEMORY_BYTES`
  are reserved.** Setting any of them in a globals or per-service
  `[env]` table is now a config-load error so an operator override
  cannot shadow the detected values.

## v0.2.0

### Features

- **`replicas = N` runs N supervised copies of a service.** Each
  replica is a first-class Runner with its own PID, crash budget,
  and `<name>/<index>` row in `zpctl status`. Replicas share the
  log file by default; opt in to per-replica files with `{index}`
  in the path. `ZPINIT_REPLICA_INDEX` is injected into env. zpctl
  bare-name fans out; `svc/N` targets one. Listener workloads share
  a port via `reusePort` (Node >= 22.12.0, Bun, Deno); see the
  README's "Node.js clustering" section.

- **`zpinit --doctor` pre-flight environment audit.** Read-only
  superset of `--check-config`: validates services, resolves each
  `command[0]` on PATH or as an absolute path, reports
  Node/Bun/Deno versions, warns when a service with `replicas > 1`
  uses a node binary below the 22.12.0 reusePort floor, and reports
  whether a zpinit instance is already on the socket. Exits 0/1/2
  for OK/FAIL/WARN-only; `--doctor-quiet` suppresses OK rows.

### Security

- **`ZPINIT_REPLICA_INDEX` is reserved.** Setting this key in any
  `[env]` table is now a config-load error: an operator override
  would shadow every replica's identity with one static value,
  breaking sharding and log attribution.

### Bug Fixes

- **Boot no longer hangs when a child wedges in uninterruptible
  kernel sleep.** `entrypoint.runOne` and the readiness prober both
  blocked indefinitely on the child's reap channel after SIGKILL; a
  `D`-state child (wedged NFS, broken FUSE) cannot be killed until
  its syscall completes. Both sites now cap the post-kill wait at
  5s.

- **`exit_code_from` retarget on reload no longer risks shutting
  the supervisor down for the wrong service.** Canceling the old
  watcher did not synchronize with its progress; if the old target
  reached terminal state during the reload, the stale goroutine
  could fire shutdown. Watcher installations now carry a generation
  counter and re-check it under the lock before firing.

- **`zpctl tail` no longer emits a leading partial log line.** The
  8KB window almost always starts mid-line; the leading fragment is
  now trimmed at the first newline when the window starts past
  offset 0.

- **Replica shutdown no longer scales linearly with replica count.**
  `stopAll` and reload remove/restart now signal all replicas of one
  filename in parallel (within-group) while preserving reverse
  filename order between groups. `replicas = 64` with a stuck
  service no longer burns ~16 min of shutdown budget.
  `ShutdownBudget` counts one `(stop_timeout + reapGrace)` per
  logical service.

## v0.1.2

### Features

- **Empty config + no CMD now stays alive in supervise mode.**
  Previously bailed with `nothing to do`. zpinit now enters
  supervise mode with zero runners so an operator can drop service
  files into `/etc/zpinit/services/` and bring them up with `zpctl
  reread` / `update` (or SIGHUP). Boot log says `no services
  configured; control socket up, waiting for reload`.

- **Published image is now a playground.** `docker run -it
  ghcr.io/0ploy/zpinit` starts zpinit as PID 1 with no services so
  you can `docker exec` in, install software, and try the supervisor
  live. `bash` and `curl` are pre-installed. Binary-delivery use
  (`COPY --from=…`) is unchanged: the layer copy ignores the new
  `ENTRYPOINT` and the extra apk packages.

- **`zpctl update` now prints what it actually did.** Previously
  responded with a bare `ok`. Now emits the same per-service lines
  as `reread`, in past tense: `+ nginx (started)`, `~ php-fpm
  (restarted)`, `- old-worker (stopped)`, or `no changes`.

- **zpinit auto-creates `/etc/zpinit/services/` and
  `/etc/zpinit/entrypoint.d/` on boot.** A fresh image no longer
  needs operators to mkdir the layout first. Skipped when `--config`
  is passed explicitly so a typo still fails loud.

## v0.1.1

### Bug Fixes

- **`zpinit` no longer requires `/etc/zpinit/` to exist for wrap
  mode.** When `--config` is not passed explicitly and the default
  dir is missing, zpinit now logs `no config dir; running with
  built-in defaults` and execs the CMD. An explicit `--config` to a
  missing path is still a hard error.

- **`zpctl help` (and `--help` / `-h`) prints local usage without
  dialing the daemon.** Previously dialed unconditionally and failed
  with `connect: no such file or directory` in pre-zpinit shells.
  The three help variants now answer locally and exit 0.

- **Auto-create the parent directory of `[log].stdout` /
  `[log].stderr` at spawn time.** Previously a config like
  `stdout = "/var/log/zpinit/foo.out"` failed to spawn unless the
  operator shipped a per-image `mklogdir.sh`. zpinit now
  `MkdirAll`s the parent (mode 0755) just before opening. Only
  paths the operator named in `[log]` are ever mkdir'd, and the
  `O_NOFOLLOW` leaf check still gates the open.

## v0.1.0

Initial release.

### Three-mode supervisor in one static binary

zpinit covers what supervisord, tini, and per-image
`docker-entrypoint.sh` do today, with one mental model:

- **Single-process mode.** No `services/`, CMD provided. zpinit
  validates config, then `syscall.Exec`s the CMD; zpinit is gone
  after the exec and the CMD becomes PID 1.
- **Setup-then-run mode.** Same as above, plus `entrypoint.d/`
  scripts run in lexicographic order before the exec. Per-script
  timeouts, zombies drained between steps, optional
  `entrypoint_on_failure = "continue"`. Scripts can write to
  `/run/zpinit/env` to hand variables forward.
- **Manage-services mode.** No CMD. zpinit reads
  `/etc/zpinit/services/*.toml`, starts each service in filename
  order with optional readiness probes gating the next, supervises
  restarts with backoff, and stays around as PID 1.

The mode is decided by whether a CMD is supplied; one image gets all
three uses without rebuilds.

### PID-1 essentials

- Single `wait4(-1, WNOHANG)` reaper loop dispatched by PID to
  per-service exit channels (tini's pattern); fast-dying child
  registration is race-free via spawn-tracked mutex.
- SIGTERM/SIGINT forwarded to children, SIGHUP triggers config
  reload, signals coalesced and serialized.
- Graceful shutdown stops services in reverse filename order with
  per-service SIGKILL escalation; the wait budget is recomputed at
  signal time so reload-added services or bumped `stop_timeout`s
  are always covered.

### Configuration

- TOML schema for globals and per-service `services/*.toml`. No env
  interpolation, no priority numbers, no dependency graphs: ordering
  is filename order plus readiness.
- `--check-config` validates the whole tree in one pass and prints
  all errors at once.
- `[env]` injects variables into the CMD or service without
  polluting container env (invisible to `docker exec`).
- `[globals.env]` is a fleet-wide default applied to all services;
  reloadable.
- Per-service `[ready]` probe gates the next service in filename
  order until it exits 0.
- Per-service `[log]` redirects stdout/stderr at spawn time.
  `inherit` (default) passes container FDs; a path opens with
  `O_APPEND|O_NOFOLLOW`.

### Reload without restart

`SIGHUP` (or `zpctl update`) re-reads the config, diffs against the
running set, and applies in filename order:

- New file: start.
- Removed file: graceful stop.
- Changed content: restart (unless `reloadable = false`).
- Renamed file: remove + add.
- Changed `[globals.env]`: every reloadable service is restarted.

`zpctl reread` previews the diff without applying.

### `zpctl` operator client

Talks to zpinit over `/run/zpinit.sock`. State names match
supervisorctl so existing muscle memory transfers.

```
zpctl status [service]
zpctl start | stop | restart [service]
zpctl signal <service> <SIG>
zpctl pid [service]
zpctl tail <service>
zpctl update | reread
zpctl shutdown
zpctl help
```

### Security posture

- Control socket bound under `umask 0o077`, then `chmod 0600` — no
  window where the socket has looser perms.
- Every accepted connection gated by `SO_PEERCRED`: peer UID must
  equal the daemon's effective UID. Non-root processes can't use
  zpctl even with filesystem access.
- Service log files open with `O_NOFOLLOW`; symlinked leaves
  rejected so a planted symlink cannot redirect writes.
- Wire-protocol responses sanitize CR/LF and lone-`.` lines so
  service-controlled log content cannot split frames.

### Build, release, and distribution

- Linux-only static binaries: `zpinit` and `zpctl`, amd64 and arm64.
- Multiarch image at `ghcr.io/0ploy/zpinit` tagged `:latest`,
  `:vX.Y.Z`, `:vX.Y`. Alpine-based, usable for `COPY --from=…` and
  for `docker run --rm -it … sh` standalone use.
- CI runs `go test`, `go vet`, `gofmt`, integration tests, and a
  `make build` version-string smoke test.
- Release workflow on `v*` tags: builds binaries + checksums, pushes
  the multiarch image, and assembles the release body from this
  file's latest section.
- All GitHub Actions in both workflows pinned to commit SHAs (not
  tags) so a compromised upstream cannot inject code into our
  pipeline.
