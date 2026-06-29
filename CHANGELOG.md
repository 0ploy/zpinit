# Changelog

## Unreleased

### Fixed

- **One malformed service file no longer takes down the whole
  directory.** Service files are now parsed and validated
  independently: a file with a stray `replicas = ""`, an unknown key,
  or any other parse/validation error is skipped with its exact error
  while every other valid service still loads. Previously a single bad
  file aborted the entire load, so unrelated, healthy services failed
  to start and `zpctl update` / `reread` refused the whole batch.
  Daemon boot and SIGHUP log the skipped file and continue (a typo
  never crashes PID 1); `zpctl update`, `update NAME`, `reread`,
  `--check-config`, `plan`, and `doctor` report each skipped file and
  exit non-zero so Puppet/CI notice. On `update`, a service whose file
  just became unparseable keeps running with its last-good config
  instead of being torn down. Whole-config errors (bad `zpinit.toml`,
  name collisions, `exit_code_from` to a missing service) stay fatal.

## v0.5.0

### Features

- **`zpctl status --json` for machine consumers.** Emits one compact
  JSON object per line (NDJSON), one per replica, with `name`,
  `service`, `replica_index`, `state`, `pid`, `uptime_seconds`,
  `total_spawns`, and `last_exit`. Add `--verbose` to include the
  `/proc` fields (`rss_bytes`, `cpu_seconds`, `fds`) for live
  processes. Tools no longer have to parse the fixed-width human
  columns; plain `--json` stays lock-only and cheap to poll.

- **`zpctl start --wait` / `restart --wait` block until ready.** The
  command returns only once the service is `RUNNING` with its
  `[ready]` probe passed, or it reaches a terminal/FATAL state (which
  exits non-zero). A service that crash-loops to FATAL no longer
  reports success, so a deploy or provisioning step doesn't mistake
  "spawned" for "converged". Bounded by `boot_timeout` and the
  probe's `timeout`.

- **`zpctl resolve NAME` locates a service's source file.** Prints the
  absolute TOML path and whether the service is currently enabled, as
  one JSON line. It scans the services dir fresh so it also resolves
  files parked with the `.disabled` convention, which the running
  config doesn't know about. Lets external tooling find the file
  without reimplementing name resolution.

- **`zpctl update NAME [NAME...]` scopes a reload to named services.**
  Applies only those services' add/remove/restart actions, so toggling
  one service can't incidentally start or stop unrelated services
  whose files changed out of band. Global `[env]` changes are deferred
  (they would restart every reloadable service); run `zpctl update`
  with no arguments to apply them.

- **`ZPINIT_SOCKET` selects the control socket for `zpctl`.**
  Precedence is `--socket PATH`, then `ZPINIT_SOCKET`, then
  `/run/zpinit.sock`. A tool that shells out to `zpctl` repeatedly can
  set it once instead of threading `--socket` through every call.

### Changes

- **Stable `zpctl` exit codes.** `0` success, `1` operation failed,
  `2` daemon unreachable, `3` unknown service. Unknown-service errors
  now return `3` (previously `1`), so a machine consumer can treat a
  missing service as stopped/absent rather than a hard failure. The
  other codes are unchanged.

## v0.4.0

### Features

- **`zpctl ready` reports stack readiness.** Exits 0 iff every
  selected service is `RUNNING` and either has no `[ready]` probe
  or has passed it; non-zero with per-service reasons in the body
  otherwise. The check schedulers, healthchecks, and CI deploys
  reach for without zpinit having to grow an HTTP endpoint.

- **`zpctl status --verbose` enriches status rows.** Each line
  picks up `rss`, `cpu` time, `fds`, lifetime `spawns`, and
  last-exit reason from `/proc`. Pure read; this is a human-
  driven command, not a polling target. Linux only (non-linux
  builds emit the row without the `/proc`-derived fields).

- **`zpctl tail --follow` streams new log lines.** Keeps the
  connection open and emits body lines as they're appended,
  including across logrotate-style file rotation (detected via
  inode change). Ctrl-C or any client disconnect ends the stream
  cleanly. The control protocol gained a small streaming
  extension (`WriteStatusLine` / `WriteBodyLine` / `WriteEnd` on
  the server side, `ReadStatusLine` / `ReadBodyLine` on the
  client side) so new long-running verbs can reuse the shape.

- **`zpinit --plan` prints the resolved boot plan.** Loads the
  config, detects the CPU/memory budget, resolves
  `replicas = "auto"` against the current snapshot, expands per-
  replica log paths, and writes a human-readable plan to stdout.
  No exec, no spawn, no entrypoint.d execution. CI scripts can
  diff this output across image versions to catch unexpected
  boot-plan drift; operators learning a new config get one
  authoritative view of what would actually happen.

- **Boot banner.** zpinit now prints one stderr line before
  dispatch:
  `[zpinit 0.4.0] mode=supervise services=4 cpu=8 memory=8GiB`
  Mode 1 (wrap) shows `argv=` instead of `services=`. Set
  `ZPINIT_NO_BANNER=1` to suppress.

- **Service files starting with `.` or ending in `.disabled` are
  skipped.** Mirrors the `entrypoint.d/` hidden/disabled
  convention. Operators can park a service out of rotation with
  `mv 20_worker.toml 20_worker.toml.disabled` and editor swap
  files (`.foo.toml.swp`-style) no longer surprise `zpctl
  reread` mid-edit.

- **Backoff has ±10% per-replica jitter.** Replicas of a service
  that crash together (e.g. their shared upstream went down) no
  longer synchronize their retry pulses on the doubling backoff
  schedule, which used to thunder-herd the recovering dependency
  on every retry. Each replica's jitter sequence is deterministic
  (seeded from its replica index), so logs and reproductions stay
  predictable.

### Bug Fixes

- **`zpctl reload` surfaces non-zero `reload_command` exits.**
  A `reload_command` exiting non-zero (bad nginx config, missing
  file, etc.) was previously logged only, with `zpctl` reporting
  success. CI pipelines running `zpctl reload svc &&
  deploy_next_step` therefore advanced even on broken reloads.
  The reload now returns a per-target error like `svc:
  reload_command exited 1 (service still running)` and the wire
  response sets a non-zero exit. The supervised service itself is
  unaffected (it was never stopped); the error text says so.

- **`zpctl reload` and `zpctl restart` body lines reflect actual
  per-target outcome.** Multi-target restart no longer drops N-1
  errors onto the floor when several services fail in the same
  call; every per-target failure now appears in the status-line
  message via `errors.Join`, and the per-target body line says
  whether that runner reloaded cleanly or which step failed.

- **Status output is tear-free.** `zpctl status` used to read each
  runner's state, PID, uptime, and crash count under four separate
  locks. Under load that could produce snapshots like `RUNNING
  pid 0` for an instant during a crash-restart cycle. A single
  `Runner.Snapshot()` accessor now collects every status field
  under one critical section.

- **Reload diff returned by `update`/SIGHUP matches what was
  applied.** Previously `cmdUpdate` computed the diff once for the
  response and `Reload` recomputed it internally; a concurrent
  reload between those two walks could cause the displayed diff
  and the applied diff to disagree. `Orchestrator.Reload` now
  returns the diff it actually applied, and the response renders
  from that.

- **Reload-on-change goroutines join the supervisor wait group.**
  A SIGTERM that arrived during a long-running `reload_command`
  could return from `Orchestrator.Run` while a reload-on-change
  goroutine was still spawning children. The detached goroutine
  is now tracked in `o.wg`, so cleanup waits for it to bail out
  (the goroutine respects the parent context cancellation, so
  shutdown is still bounded).

- **`servicesEqual` ignores nil-vs-empty noise.** Cosmetic TOML
  edits (`env = {}` vs no `env` key, `reloadable = true` vs no
  key, `reload_on_change = []` vs no key) used to trigger phantom
  restarts on reload because `reflect.DeepEqual` treats nil and
  empty as unequal. The diff now normalizes those shapes before
  comparing, so only semantic changes restart services.

- **`removeServiceGroup` is O(N+K) instead of O(N*K).** Removing
  K runners from a registry of N used to walk the registry once
  per victim. Single-pass filter with a removeSet now does the
  job in one walk regardless of K. Negligible on small configs;
  meaningful if `MaxReplicas` ever grows.

- **`dispatchBudget` no longer over-budgets `reload`.** A
  `reload` verb with `reload_signal` configured finishes in
  microseconds (it's a `kill(2)`), but the control connection
  used to stay open with a full `stop_timeout`-shaped deadline
  per target. The budget now picks the right per-verb shape:
  skip stop budget for `reload_signal`, use
  `reload_command_timeout` for `reload_command`, full stop budget
  for everything else.

- **`Watcher.Subscribe` returns a cleanup function.** Repeated
  subscriptions (e.g. tests, or future code paths that
  re-subscribe per reload) used to leak channel entries because
  `w.subs` was append-only. Mirrors the `Runner.Observe` pattern.

- **`zpinit doctor` warns when `[ready].command` matches the
  service's main command.** Common copy-paste mistake; the probe
  then runs the entire service binary on every interval and
  either "succeeds" trivially or fails for orthogonal reasons.
  Flagged as a WARN so legitimate self-test invocations aren't
  blocked.

- **`Runner.NewRunnerForReplica` makes the spec/cfg distinction
  explicit.** Per-replica runners need `cfg` (rewritten log path,
  ZPINIT_REPLICA_INDEX env) for spawn and an unmodified `spec`
  for diff equality. Callers used to assign `r.spec` by hand
  after `NewRunner`; the new constructor takes both up front so
  the relationship can't be set wrong.

### Tests

- **CI runs `go test -race`.** The v0.3.1 review surfaced two
  data races and the fixes landed without a CI guard. Both
  unit and integration jobs now run with `-race -count=1` so a
  future regression in the watcher/reload/scale fan-out gets
  caught before merge rather than after.

## v0.3.1

### Bug Fixes

- **Watcher-driven autoscale no longer races with `Reload`.** A
  resource-watcher commit could overlap a SIGHUP or `zpctl update`,
  letting `Reload` overwrite the scaler's freshly-computed
  `Replicas.N` with the stale disk-loaded value (and vice versa
  depending on which goroutine won the lock race). `OnResourceChange`
  now holds `reloadMu` across the `SetResourceEnv →
  SetCurrentSnapshot → scaleAutoServices` triad so the two paths
  serialize cleanly.

- **Readiness-probe env no longer races the resource watcher.** The
  orchestrator's boot paths (initial boot and reload-boot) used to
  read a runner's `baseEnv` slice directly while
  `SetResourceEnv` could be writing it from the watcher fan-out — a
  data race in the strict Go memory-model sense that `go test -race`
  would flag. A new `Runner.BaseEnv()` accessor takes the runner's
  mutex; the two boot paths now go through it.

- **`boot_timeout` is now per-service at initial boot.** Previously
  the initial boot phase shared one `context.WithTimeout` across
  every service, so a service that legitimately took 50 s of a 60 s
  budget left the remaining services with 10 s combined to finish
  their readiness probes. Each service now gets its own fresh
  `boot_timeout`, matching reload-boot and the contract documented
  in CLAUDE.md.

- **`zpctl restart all` and friends parallelize within filename
  groups.** Stop / start / restart now process consecutive
  same-filename targets in parallel (matching `stopAll`'s
  parallel-within-group / serial-between-groups schedule), so
  `zpctl restart all` on a service with `replicas = 64` finishes
  in one `stop_timeout` instead of 64. Cross-service ordering is
  preserved.

- **Control-socket dispatch budget no longer over-counts replicas.**
  The deadline `handleConn` puts on a `restart all` / `stop all`
  connection used to grow linearly with each replica; with
  `replicas = 64` the daemon-side handler kept the socket open for
  tens of minutes after the client gave up. Budget is now per
  filename group, matching the actual parallel-within-group
  execution shape.

- **Manual `zpctl start` during backoff resets the crash budget.**
  Previously the crash counter survived a manual override, so
  repeatedly running `start` on a flapping service could fast-track
  it to FATAL even when the operator's intent was "fresh attempt."

## v0.3.0

### Features

- **`replicas = "auto"` lets zpinit track the detected CPU count.**
  At boot the replica count is set to the natural CPU budget; every
  debounced resource commit thereafter rebalances the runner set
  (add replicas in filename order, drop highest-indexed on scale
  down). Optional `replicas_min` and `replicas_max` clamp the
  count, with `replicas_min` doubling as a floor that can push the
  count *above* the natural CPU value for I/O-bound queue workers
  ("16 sidekiqs on a 2-CPU box"). `replicas = "auto"` implies
  `reload_on_change = ["cpu", "memory"]` so the surviving replicas
  also pick up the new env on the next spawn; set
  `reload_on_change = []` to opt out for stateless workers.
  `docker update --cpus N` (or Kubernetes in-place pod resize) now
  rescales the live workload without operator intervention.

- **Resource limit changes now trigger live reload.** A background
  watcher polls cgroup state once a second and commits a delta
  (after `scale_up_after` / `scale_down_after` debounce, defaults
  5 s / 30 s) when the exposed `ZPINIT_CPU_COUNT` or
  `ZPINIT_MEMORY_BYTES` value moves. New per-service
  `reload_on_change = ["cpu", "memory"]` opts in: zpinit fans out
  the configured reload action (signal, command, or full restart)
  to every subscriber whose dimensions intersect the change. Lets
  `docker update --cpus N` (or Kubernetes in-place pod resize)
  propagate to live workloads without operator intervention.
  Sub-integer quota wobble that doesn't move the integer floor is
  intentionally invisible.

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
