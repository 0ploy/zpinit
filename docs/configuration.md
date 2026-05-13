# Configuration

zpinit reads everything under `/etc/zpinit/`. Validate with
`zpinit --check-config /etc/zpinit/` before deploying.

## Layout

```
/etc/zpinit/
├── zpinit.toml         # global defaults; entire file is optional
├── services/
│   ├── 10_redis.toml   # one TOML file per service
│   ├── 20_php-fpm.toml
│   └── 99_worker.toml
└── entrypoint.d/
    ├── 10-fix-perms.sh # executable scripts run before any service
    └── 20-warmup.sh    # non-executable files are skipped (with a warning at --check-config)
```

**Filename order is load-bearing.** Services start in lexicographic
order of their filename. The numeric prefix (`10_`, `30-`) is stripped
from the resolved service name (`10_redis.toml` becomes service
`redis`); set `name = "..."` in the TOML to override.

Service names must match `^[a-zA-Z0-9_-]+$` and must be unique after
stripping prefixes. `--check-config` reports collisions.

## `zpinit.toml` (globals)

Every key is optional. Defaults shown.

```toml
# Behaviour when an entrypoint.d/ script exits non-zero.
# "fail" aborts the container; "continue" runs the next script anyway.
entrypoint_on_failure = "fail"

# Per-script timeout for entrypoint.d/. Slow `composer install` runs
# burn this budget, not boot_timeout.
entrypoint_script_timeout = "5m"

# Time budget for the service-boot phase (start + readiness probe per
# service, summed). Starts at the moment service-boot begins, not at
# zpinit launch. Covers the WHOLE service list, not a per-service
# budget: with many services or a slow late service, an early service
# can be denied its share. Set generously relative to the sum of
# expected boot times, or split into smaller images.
boot_timeout = "60s"

# Default signal sent to services on graceful stop.
default_stop_signal = "TERM"

# Default time a service has to exit after its stop signal before
# SIGKILL escalation.
default_stop_timeout = "10s"

# Foreground-worker pattern. "default" means "exit when all services
# are done". Set to a service name to make zpinit exit with that
# service's exit code when it terminates.
exit_code_from = "default"

# Path of the zpctl Unix socket. Must be absolute. The socket is bound
# 0700 (umask-tightened across bind so the file is never briefly
# world-accessible) and chmod'd 0600. Connecting peers are then gated
# by SO_PEERCRED: only processes running as the daemon's UID can talk
# to it. In a normal container that means root only; non-root services
# (php-fpm workers, etc.) cannot use zpctl.
control_socket = "/run/zpinit.sock"

# Fleet-wide default env. Visible to entrypoint.d scripts and to the
# wrap-mode CMD or supervised services. Not visible to `docker exec`.
# See "Globals env" below for precedence and reload semantics.
[env]
APP_ENV   = "production"
LOG_LEVEL = "info"
```

### Globals env

The `[env]` block declares fleet-wide defaults that travel via
syscall.Exec / spawn (so they reach the workload but not `docker exec`).
Keys must match `^[A-Za-z_][A-Za-z0-9_]*$`; `--check-config` validates.

**Precedence chain (lowest first):**

1. `[env]` in `zpinit.toml`. Build-time defaults baked into the image.
2. Container env: Dockerfile `ENV`, `docker run -e`, `--env-file`. An
   operator can override a baked-in default at deploy time.
3. `entrypoint.d/` writes to `/run/zpinit/env`. Boot-time runtime
   discoveries (e.g. vault fetches) override both layers above.
4. Per-service `[env]` (mode 3 only). Per-service overrides win
   everything.

**This is for defaults, not secrets.** Anything in `zpinit.toml` is
baked into the image. Use `docker run -e` from your orchestrator's
secret store, or fetch in an entrypoint script and write to
`/run/zpinit/env`.

**Reload semantics (mode 3 only).** A SIGHUP / `zpctl update` that
changes `[env]` causes every reloadable service to be restarted so the
new env reaches the next spawn. Long-running children can't be given
new env retroactively; restart is the only mechanism. Services with
`reloadable = false` keep their old env and log a warning.

**`--skip-entrypoint`** still applies `[env]`. Skipping scripts only
suppresses the `entrypoint.d/` phase; the toml layer is always
evaluated.

### Resources

Optional `[resources]` block in `zpinit.toml`. zpinit detects the
container's CPU and memory budget at boot and injects three env
variables into every child (the wrapped CMD or every supervised
service):

- `ZPINIT_CPU_COUNT` — integer floor of available CPUs, minimum 1.
- `ZPINIT_CPU_QUOTA` — fractional CPU budget, e.g. `1.5`.
- `ZPINIT_MEMORY_BYTES` — memory budget in bytes, `0` for unlimited
  or undetected.

Detection takes the min of every source it can read: cgroup v2
(`cpu.max`, `memory.max`), cgroup v1 (`cpu.cfs_quota_us` /
`cpu.cfs_period_us`, `memory.limit_in_bytes`), and `/proc/cpuinfo` /
`/proc/meminfo`. A container inside a VM is covered: cgroup limits
and the VM's kernel view both apply, whichever is smaller wins. On
bare metal or a microVM without cgroups, `/proc` is authoritative.

Apps decide whether to read the vars. nginx wrappers can map
`ZPINIT_CPU_COUNT` onto `worker_processes`; the JVM onto `-Xmx`; a
Node clustering shim onto `cluster.fork()` count. zpinit only
exposes the numbers.

Operator `[env]` tables (globals or per-service) may not set these
keys; `--check-config` rejects the override.

```toml
[resources]
# Subtracted from the detected budget before children see the env
# vars. Useful when a master process, sidecar, or zpinit itself
# needs headroom that workers should not assume is theirs.
reserve_cpu     = 0.5
reserve_memory  = "256MiB"
```

Byte sizes accept `K`/`KB`/`Ki`/`KiB` (and `M`, `G`) suffixes:
unsuffixed digits and `B`/`KB`/`MB`/`GB` use 1000-base; `Ki`/`Mi`/`Gi`
and the `iB` forms use 1024-base. `reserve_cpu` is a non-negative
float; `reserve_memory` is a non-negative byte count.

The boot-time detection is fixed for this commit; live updates land
in a later release. Watching `cpu.max` / `memory.max` for runtime
limit changes is on the roadmap.

## `services/*.toml` (one per service)

```toml
# Required. argv passed to the service. No shell, no env interpolation.
command = ["redis-server", "--daemonize", "no"]

# Optional override. Default is the filename with numeric prefix and
# .toml extension stripped.
name = "redis"

# Working directory.
cwd = "/var/lib/redis"

# Drop privileges. Names or numeric IDs.
user  = "redis"
group = "redis"

# Restart policy: "always" (default), "on-failure", or "never".
restart = "always"

# Crash backoff. Doubles from initial to max; resets after the service
# stays up for backoff_reset_after; gives up after 5 consecutive crashes
# (FATAL state).
backoff_initial     = "1s"
backoff_max         = "30s"
backoff_reset_after = "60s"

# Graceful stop. Falls back to globals if unset.
stop_signal  = "TERM"   # or "INT", "QUIT", "USR1", "HUP", ...
stop_timeout = "10s"

# Default true. Set false if the service should be left alone across
# config reloads (a long-running batch job, for example).
reloadable = true

# Number of independent supervised copies of `command`. Default 1.
# Each replica is a first-class child with its own PID, log file, and
# crash budget; ZPINIT_REPLICA_INDEX=0..N-1 is injected into each
# replica's env (only when replicas > 1). Replicas of an app that
# binds a port without SO_REUSEPORT support will collide with
# EADDRINUSE on all but the first; `zpinit --doctor` catches the
# common cases. Maximum 64 (typo guard); promotes to a config knob
# only if anyone asks. See clustering.md for the listener case and
# the PM2 comparison.
replicas = 1

# In-place reload action for `zpctl reload <name>`. At most one of the
# two may be set; both unset means `zpctl reload` falls back to a full
# stop+start cycle (so operators can always say "reload" and have it
# do the right thing per service).
#
#   reload_signal  — send a signal to the service's process group. The
#                    process keeps running; whatever it does on the
#                    signal is its own concern (nginx re-reads its
#                    config on HUP, php-fpm reloads on USR2, …).
#   reload_command — exec a one-shot command that talks to the live
#                    process via its own IPC (`nginx -s reload` over
#                    the daemon's Unix socket, for example). Inherits
#                    the service's env; stdout/stderr go to zpinit's
#                    log. Non-zero exit is logged but does not kill
#                    the service.
#
reload_signal  = "HUP"
# reload_command = ["/usr/sbin/nginx", "-s", "reload"]

# Per-service environment variables. Merged on top of inherited env.
[env]
LOG_LEVEL = "info"
DATABASE_URL = "postgres://..."

# stdout/stderr destination. "inherit" (default) writes to the
# container's stdout/stderr (the right answer for almost everything).
# A path writes to a file with O_APPEND|O_NOFOLLOW: a symlink at the
# leaf of the path is rejected at spawn time. Symlinked parent
# directories resolve normally.
#
# For replicas > 1, log paths default to a shared file: every replica
# writes to the same path. Linux O_APPEND is atomic for line-sized
# writes (<= PIPE_BUF, typically 4096 bytes), so concurrent appends
# from N replicas don't tear at line boundaries for normal log output.
#
# To get per-replica files instead, put `{index}` in the path; it
# expands to 0..N-1:
#   "/var/log/consumer-{index}.log" -> "/var/log/consumer-0.log", ...
# "inherit" is unchanged across replicas.
[log]
stdout = "inherit"
stderr = "inherit"

# Optional readiness probe. Until this exits 0, the next service in
# filename order does not start.
[ready]
command  = ["redis-cli", "ping"]
interval = "500ms"   # delay between probe attempts
timeout  = "30s"     # give up after this long
on_timeout = "fail"  # "fail" aborts boot; "continue" proceeds anyway
```

## `entrypoint.d/`

Plain executables (any language with a shebang). zpinit runs them in
filename order, each with `entrypoint_script_timeout` applied. A
non-zero exit is fatal unless `entrypoint_on_failure = "continue"`.

Files matching `.*` (dotfiles) or ending in `.disabled` are skipped
silently. Non-executable files are skipped with a warning at
`--check-config`.

Scripts can write key=value lines to `/run/zpinit/env` to export env
vars to all services. (Test-only: `ZPINIT_ENV_FILE` overrides the
path.)

## Validation

```sh
zpinit --check-config /etc/zpinit/
```

Loads everything, applies defaults, validates, and either prints a
one-line OK summary or every error found in one pass. Exit 0 / 1.

`--check-config` validates:

- TOML syntax and unknown keys (typos surface here).
- Service name uniqueness after prefix stripping.
- Service name pattern (`^[a-zA-Z0-9_-]+$`).
- `command` is non-empty.
- `restart`, `entrypoint_on_failure`, `[ready].on_timeout` are valid.
- `default_stop_signal` and per-service `stop_signal` are recognised.
- `exit_code_from` references an existing service (or is `"default"`).
  Pointing it at a service with `replicas > 1` is rejected (ambiguous).
- `replicas` is in `[1, 64]`.
- `entrypoint.d/` files are executable (warning, not error).
- `control_socket` is an absolute path.

For a deeper pre-flight audit (filesystem writability, binary
resolution, runtime version checks, whether a zpinit instance is
already running), use `zpinit --doctor /etc/zpinit/` instead — it's a
superset of `--check-config`.
