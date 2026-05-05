# zpinit

A single static Go binary that runs as PID 1 in your Docker container,
replacing the three tools most images stitch together to do that job:
**tini** (PID-1 reaper), **a hand-written `docker-entrypoint.sh`** (setup
before the workload), and **supervisord** (multi-process supervision).

One binary. One mental model. ~3 MB on disk, no Python runtime, no shell
wrappers. The same image works in production, as a debug shell, and as
a one-off task runner without flags or env vars to flip modes.

## Why we built this

Every container image we ship eventually grows the same shape:

- A reaper to clean up zombie processes (tini, dumb-init, or hope).
- A `docker-entrypoint.sh` that runs `composer install`, applies
  migrations, fixes permissions, then `exec`s the real CMD.
- supervisord for images that run more than one process — php-fpm +
  nginx, php-fpm + a worker, redis + an app, and so on.

Three tools, three configuration formats, three mental models.
supervisord drags ~30–50 MB of Python into every image and isn't even
real PID 1 (it still wants tini in front). The entrypoint script is
bespoke per image and grows organically until nobody trusts it.

Updating any of them across our fleet of 130+ shop images is days of
work and zero confidence.

zpinit folds all three into one Go binary. Same supervisor in every
image, same config shape, same operator commands, ~3 MB.

## What it does

On startup, zpinit always:

1. Runs every executable file in `/etc/zpinit/entrypoint.d/` in
   lexicographic order. (Setup phase. Replaces `docker-entrypoint.sh`.)
2. Decides what to run next based on whether a CMD was provided:

| Setup                                              | Mode      | Behaviour                                                       |
| -------------------------------------------------- | --------- | --------------------------------------------------------------- |
| no CMD, populated `services/`                      | supervise | Run services, manage their lifecycle (replaces supervisord).    |
| any CMD (`docker run image bash`, `… php cli …`)   | wrap      | `exec` the CMD as PID 1, ignore `services/` (replaces tini).    |
| no CMD, empty `services/`                          | error     | Exit non-zero with a clear "nothing to do" message.             |

The same image is your daemon, your debug shell, and your task runner:

```sh
docker run myimage                            # production: services run, supervised
docker run -it myimage bash                   # debug: setup runs, then a shell — no services
docker run --rm myimage php bin/console fix   # one-off task with all the setup, no daemons
```

This is the killer feature — no separate "debug" image, no flags, no
env vars to flip modes.

## How to use it

### Dockerfile pattern

```dockerfile
FROM debian:stable-slim

RUN apt-get update && apt-get install -y nginx redis-server php-fpm \
    && rm -rf /var/lib/apt/lists/*

# Drop in zpinit (build from source, or copy from your published image).
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpinit /usr/local/bin/
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpctl  /usr/local/bin/

# Drop in setup scripts and service definitions.
COPY entrypoint.d/ /etc/zpinit/entrypoint.d/
COPY services/     /etc/zpinit/services/

ENTRYPOINT ["zpinit"]
# No CMD: supervise mode by default.
```

That's it. `docker run myimage` supervises in production;
`docker run -it myimage bash` still drops into a usable shell with all
the entrypoint setup applied.

### Config layout

```
/etc/zpinit/
├── zpinit.toml         # global defaults — all optional
├── services/           # one TOML per service
│   ├── 10_redis.toml
│   ├── 20_php-fpm.toml
│   └── 99_worker.toml
└── entrypoint.d/       # executable scripts; non-executable ones are skipped
    ├── 10-fix-perms.sh
    └── 20-warmup.sh
```

Filename order determines start order. Numeric prefixes (`10_`, `20_`,
`99_`) are stripped from the resolved service name (`10_redis.toml` →
`redis`); set `name = "..."` in the TOML to override.

### A minimal service file

```toml
# services/10_redis.toml
command = ["redis-server", "--daemonize", "no"]
restart = "always"

[ready]
command  = ["redis-cli", "ping"]
interval = "500ms"
timeout  = "30s"
```

The `[ready]` block is optional. When set, zpinit runs the probe in a
loop after starting redis and waits until it exits 0 before starting
the next service. This is how you sequence "redis must be up before
php-fpm starts."

The full schema (env, cwd, user/group, log destinations, backoff timing,
stop signal/timeout, reload behavior) lives in
`internal/config/config.go`.

### Validating before deploy

```sh
zpinit --check-config /etc/zpinit/
```

Loads everything, applies defaults, validates, and either prints a
one-line OK summary or every error found in one pass. Exit 0 / 1.

## Common patterns

### Single-process image (drop-in tini replacement)

You have an image with a single workload — a Go server, a long-running
PHP CLI, anything. You used to use tini.

```dockerfile
ENTRYPOINT ["zpinit"]
CMD ["my-server", "--port", "8080"]
```

No `services/` dir. zpinit runs `entrypoint.d/` (use it for migrations,
config rendering, perm fixes), then `exec`s `my-server` as PID 1. Any
orphans the workload leaves behind get reaped.

### Multi-process image (drop-in supervisord replacement)

php-fpm + nginx + redis, supervisord-style.

```toml
# services/10_redis.toml
command = ["redis-server", "--daemonize", "no"]
[ready]
command = ["redis-cli", "ping"]
```

```toml
# services/20_php-fpm.toml
command = ["php-fpm", "-F"]
[ready]
command = ["sh", "-c", "test -S /run/php/php-fpm.sock"]
```

```toml
# services/30_nginx.toml
command = ["nginx", "-g", "daemon off;"]
```

```dockerfile
ENTRYPOINT ["zpinit"]
# No CMD — supervise mode.
```

Boot order is by filename: redis first, wait for `redis-cli ping` to
return 0, then php-fpm, wait for the socket, then nginx.

### Foreground worker with supporting daemons

The Symfony pattern: php-fpm has to be running, but the *real* job is a
`messenger:consume` worker that should end the container when it exits
(so Kubernetes / Nomad / your scheduler can take over).

```toml
# zpinit.toml
exit_code_from = "worker"
```

```toml
# services/10_php-fpm.toml
command = ["php-fpm", "-F"]
restart = "always"
```

```toml
# services/99_worker.toml
name    = "worker"
command = ["php", "bin/console", "messenger:consume"]
restart = "never"
```

When `worker` exits, zpinit gracefully stops php-fpm and exits with the
worker's exit code.

There's deliberately no separate "supervise + main task" mode. Express
the foreground job as a service with `restart = "never"` plus
`exit_code_from`, and you get the same behavior with one consistent
config shape.

### Reload without restart

`SIGHUP` to zpinit (or `zpctl update`) re-reads `/etc/zpinit/`, diffs
against the running set, and applies:

- New file → start the new service.
- Removed file → graceful stop.
- Changed content → restart (unless `reloadable = false`).
- Renamed file → remove + add.

`zpctl reread` previews the diff without applying.

### Operator commands

`zpctl` talks to zpinit over `/run/zpinit.sock`. State names match
supervisorctl exactly so existing muscle memory transfers.

```sh
zpctl status                  # all services
zpctl status redis            # one
zpctl start redis             # or: start all
zpctl stop redis              # or: stop all
zpctl restart redis
zpctl signal redis HUP        # nginx-style "reload your own config"
zpctl pid                     # zpinit's PID
zpctl pid redis               # the service's PID
zpctl tail redis              # snapshot of file-logged stdout
zpctl update                  # apply config changes (= SIGHUP)
zpctl reread                  # dry-run config diff
zpctl shutdown
zpctl help
```

## Architecture

A single static Go binary, CGO disabled, built with `-trimpath`. ~3 MB.
Linux-only in production (uses `Pdeathsig`, `Setpgid`, `/proc`); macOS
dev compiles via build tags but doesn't exercise PID-1 paths.

| Package                | Role                                                                                           |
| ---------------------- | ---------------------------------------------------------------------------------------------- |
| `cmd/zpinit`           | Supervisor binary. Mode detection, signal loop, dispatch.                                      |
| `cmd/zpctl`            | Thin control client.                                                                           |
| `internal/config`      | TOML loading, defaults, validation, `--check-config`.                                          |
| `internal/entrypoint`  | `entrypoint.d/` runner, env-file propagation.                                                  |
| `internal/reaper`      | Centralized `wait4(-1, WNOHANG)` loop with PID dispatch.                                       |
| `internal/service`     | Process spawn with SysProcAttr, credentials, log destinations.                                 |
| `internal/supervisor`  | Per-service state machine, orchestrator (boot, readiness, reload, shutdown), control server.   |
| `internal/ctlproto`    | Wire protocol between zpinit and zpctl.                                                        |

Per-service state machine:

```
pending → starting → running → stopping → stopped
                 ↘ backoff ↗
                 ↘ fatal
```

Backoff doubles from `backoff_initial` to `backoff_max`, resets after
the service stays up for `backoff_reset_after`, and gives up after 5
consecutive crashes (FATAL).

## Design decisions

**CMD wins over services.** When a CMD is provided, zpinit `exec`s it
and ignores `services/` entirely. Same image, three behaviors
(production / debug / task) without flags. We deliberately rejected
adding a separate "supervise + main task" mode — express foreground
tasks as a service with `restart = "never"` plus `exit_code_from`.

**One Wait4 site.** zpinit reaps every child via a single
`wait4(-1, WNOHANG)` loop dispatched per PID — never `cmd.Wait()` per
process. The two race against each other; whichever the kernel
satisfies first wins, the loser gets `ECHILD`, and you lose the exit
code. tini does it the same way.

**Filename ordering, not dependency graphs.** Services start in
lexicographic order; readiness probes block the next start. This is
enough at our scale — adding a real DAG would mean another DSL to learn
for marginal benefit.

**Readiness via a separate probe command.** Rather than parsing log
output or sniffing ports, you tell zpinit how to ask the service if
it's ready (`redis-cli ping`, `mysqladmin ping`,
`curl -f http://127.0.0.1/health`). Most reliable, most explicit.

**FD inheritance for stdout/stderr, never pipes.** Pipes mean we'd have
to drain them. Drain too slowly and the service deadlocks on writes.
Inherit fds — point them at the terminal, at a file, or at whatever the
kernel decides — and the service writes directly without zpinit in the
data path.

**Plaintext control protocol.** zpctl talks to zpinit over a Unix
socket with a line-based plaintext format. Operators debug with `nc` or
`socat`. JSON would have been "nicer" but worse to debug live.

## Philosophy

**Replace three tools with one mental model.** When every image uses
the same supervisor with the same config shape, fleet-wide changes go
from "rewrite N entrypoint scripts" to "edit the TOML."

**Pragmatic over general-purpose.** zpinit is built for *our* images,
not as a drop-in for anyone's. It doesn't try to handle every Linux
init scenario, just the ones our fleet actually has.

**No magic.** No template engines, no env interpolation in configs, no
auto-discovery. If you need different values in dev vs prod, generate
the config from your provisioning. The runtime stays predictable.

**Production reliability is load-bearing.** The failure modes that
actually bite — zombie leaks, stuck shutdowns, missed orphan reaping,
race conditions in startup ordering — get explicit attention in the
design and tests. Each is the kind of bug that takes a week to surface
and degrades hosts silently.

**Deliberately not in scope:** log rotation (use logrotate or stdout →
host logging), log capture via pipes, service dependency graphs, env
interpolation, web UI / metrics endpoint, Windows / FreeBSD support,
interactive `zpctl fg`. Feature requests matching one of these get
pushed back.

## Development

```sh
make build       # static binaries to bin/
make test        # unit tests (Linux + macOS)
make integration # full-binary integration tests (Linux only)
make lint        # gofmt + go vet
```

CI runs unit on every push (Linux + macOS), integration on PRs and
pushes to main.

The implementation history is in `git log --oneline` — each phase is
one commit with a detailed message explaining what landed and why.
Agent-facing project notes live in `CLAUDE.md`.

## License

MIT. See `LICENSE`.
