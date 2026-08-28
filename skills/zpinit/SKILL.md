---
name: zpinit
description: >-
  Set up and operate zpinit, a single ~3 MB static Go binary that runs as
  PID 1 in Docker containers and replaces tini, docker-entrypoint.sh,
  supervisord, and PM2. Use this skill whenever you write or modify a
  Dockerfile or container image that needs an init process, zombie reaping,
  entrypoint/setup scripts, more than one process in one container
  (nginx + php-fpm, app + worker), supervised restarts with backoff,
  readiness probes, or Node.js clustering via replicas. Also use it whenever
  the user mentions zpinit, zpctl, /etc/zpinit, or wants to replace
  supervisord, tini, dumb-init, PM2, or a hand-rolled docker-entrypoint.sh.
---

# zpinit

zpinit is one static Linux binary that runs as PID 1 inside a Docker
container. It reaps zombies, runs setup scripts, supervises services
(restart with capped-exponential backoff, readiness probes, live config
reload), forwards signals, and handles graceful shutdown. Configuration
is TOML under `/etc/zpinit/`. It replaces four tools at once: tini,
`docker-entrypoint.sh`, supervisord, and PM2.

## Install into an image

Copy the binary from the published image. It is static, so it works in
any base image (alpine, debian, ubuntu, distroless):

```dockerfile
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpinit /usr/local/bin/
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpctl  /usr/local/bin/
ENTRYPOINT ["zpinit"]
```

`zpctl` is only needed for supervise mode (mode 3 below). Pin a version
tag instead of `latest` for reproducible builds.

## Mode selection: one rule

zpinit picks its mode from the CMD at startup. No flags.

**A non-empty CMD means zpinit execs it. No CMD means supervise mode.**

`/etc/zpinit/entrypoint.d/` scripts always run first, in every mode.

| Mode | Trigger | Replaces |
| --- | --- | --- |
| 1. Single process | CMD set, no setup scripts | tini |
| 2. Setup, then run | CMD set, `entrypoint.d/` populated | docker-entrypoint.sh |
| 3. Manage services | No CMD, `services/*.toml` present | supervisord, PM2 |

Do not try to combine "supervise services" with "run a main task" via
CMD; that combination does not exist. Express a foreground worker as a
service with `restart = "never"` plus `exit_code_from = "<name>"` in
`zpinit.toml` (the container then exits with that service's exit code).

Only set `exit_code_from` when one service really does own the
container's lifetime. Without it, once boot has completed nothing a
service does ends the container: it crashes, goes FATAL, or gets
stopped, and zpinit stays PID 1 until it is signaled or `zpctl
shutdown` runs. That is what a dev or debug container wants, so
`docker exec` and `zpctl restart` survive a dead app.

With it, the container exits when that one service ends **on its own**:
a clean or failing exit under `restart = "never"`, or a crash-loop into
FATAL under a restarting policy (both policies are supported). Operator
actions never count: `zpctl restart` and `zpctl stop` on the named
service leave the container up. The other way a service can end the
container is failing its INITIAL boot, which is independent of this key
(see `on_boot_failure` below).

## Mode 1: single process

zpinit validates config, then `syscall.Exec`s the CMD, which takes over
as PID 1:

```dockerfile
FROM alpine:latest
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpinit /usr/local/bin/
COPY my-app /usr/local/bin/
ENTRYPOINT ["zpinit"]
CMD ["my-app", "--port", "8080"]
```

Note: after the exec, zpinit is gone and nothing reaps children. If the
workload spawns children it doesn't reap, use mode 3 with a single
service entry instead; zpinit then stays as PID 1 and reaps.

## Mode 2: setup, then run

Executable scripts in `/etc/zpinit/entrypoint.d/` run sequentially in
filename order (migrations, permission fixes, config rendering), then
zpinit execs the CMD:

```dockerfile
FROM node:20-alpine
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpinit /usr/local/bin/
COPY entrypoint.d/ /etc/zpinit/entrypoint.d/
COPY dist/ /app/dist/
ENTRYPOINT ["zpinit"]
CMD ["node", "/app/dist/server.js"]
```

Rules for entrypoint scripts:

- Any language with a shebang; the file must be executable.
- Non-zero exit aborts boot (set `entrypoint_on_failure = "continue"`
  in `zpinit.toml` to override).
- Each script gets `entrypoint_script_timeout` (default `"5m"`, set in
  `zpinit.toml`); raise it for slow migrations or `composer install`.
- Append `KEY=value` lines to `/run/zpinit/env` to export variables to
  later scripts, the CMD, and all services.
- Files starting with `.` or ending in `.disabled` are skipped.

### Waiting for an external dependency (database, broker)

Use an entrypoint script; the script timeout bounds the wait and a
timeout aborts boot so the container restart policy retries:

```sh
#!/bin/sh
# entrypoint.d/05-wait-mysql.sh
set -eu
until nc -z mysql 3306 2>/dev/null; do sleep 1; done
```

If only some services need the dependency and the rest should boot in
parallel with the wait (mode 3), use a one-off wait service ordered
before the dependent one instead: `services/05_wait-mysql.toml` with
`command = ["sh", "-c", "until nc -z mysql 3306; do sleep 1; done"]`
and `restart = "never"`. This variant runs under the per-service
`boot_timeout` (default 60s), so raise that if the dependency can be
slow. Do NOT rely on the app crash-looping until the dependency is up:
after 5 consecutive crashes zpinit marks the service FATAL and stops
retrying.

## Mode 3: manage services

No CMD in the Dockerfile. zpinit reads `/etc/zpinit/services/*.toml`,
starts each service in filename order (the numeric prefix encodes
dependency order), and stays resident as supervisor:

```dockerfile
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends nginx php8.3-fpm \
 && rm -rf /var/lib/apt/lists/*
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpinit /usr/local/bin/
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpctl  /usr/local/bin/
COPY services/ /etc/zpinit/services/
RUN zpinit --check-config /etc/zpinit/
ENTRYPOINT ["zpinit"]
# No CMD: supervise mode.
```

One TOML file per service, e.g. `services/10_php-fpm.toml`:

```toml
command = ["/usr/sbin/php-fpm8.3", "-F"]   # argv; no shell, no interpolation
restart = "always"                          # or "on-failure" | "never"

stop_signal  = "TERM"                       # default; "TERM" and "SIGTERM" both work
stop_timeout = "10s"                        # default; then SIGKILL escalation

on_boot_failure = "fail"                    # default: failing initial boot exits the
                                            # container. "continue" keeps PID 1 alive

[ready]                                     # optional; gates the NEXT service
command    = ["sh", "-c", "test -S /run/php/php8.3-fpm.sock"]
interval   = "200ms"                        # delay between attempts
timeout    = "10s"                          # give up after this long
on_timeout = "fail"                         # default: abort boot; "continue" proceeds
                                            # (what an aborted boot DOES is
                                            #  on_boot_failure's job, above)

[env]                                       # per-service overrides
LOG_LEVEL = "info"

[log]                                       # default "inherit" = container stdout/stderr
stdout = "inherit"
stderr = "inherit"
```

Key facts when writing service files:

- `command` must run in the **foreground** (`nginx -g "daemon off;"`,
  `php-fpm -F`, `redis-server --daemonize no`).
- `command` is argv only. For shell features use
  `["sh", "-c", "..."]` explicitly.
- The service name is the filename with numeric prefix (`10_` or
  `10-`) and `.toml` stripped (`10_redis.toml` becomes `redis`); set
  `name = "..."` to override.
- There is no dependency graph: filename order plus `[ready]` probes is
  the ordering mechanism. Each service gets its own `boot_timeout`
  window (default `"60s"`, global) for start plus readiness probe.
- A crash restarts the service with exponential backoff; 5 consecutive
  crashes put it in FATAL state. Neither ends the container (unless
  `exit_code_from` names that service, in which case FATAL does).
- A service failing its INITIAL boot DOES end the container, exit 1,
  whatever `exit_code_from` says. That is the default so an
  orchestrator sees a container that could not start. Add
  `on_boot_failure = "continue"` to a service to opt out: the failure
  is logged, the service stays visible in `zpctl status`, later
  services still boot, and PID 1 keeps running. Use it in dev images,
  where the operator needs `docker exec` to get into a broken
  container and repair it in place.
- File log paths get their parent directory auto-created; a symlink at
  the file itself is rejected. zpinit does not create other runtime
  dirs (e.g. `/run/php` for a socket): do that in `entrypoint.d/`.
- Other useful keys: `cwd`, `user`/`group` (privilege drop),
  `reload_signal`/`reload_command` (used by `zpctl reload`),
  `reloadable = false` to exempt a service from config reloads.
- Global defaults (`boot_timeout`, `default_stop_signal`,
  `default_stop_timeout`, fleet-wide `[env]`, `exit_code_from`) live in
  the optional `/etc/zpinit/zpinit.toml`. Anything in TOML `[env]` is
  baked into the image: not for secrets; pass those via `docker run -e`
  or fetch in an entrypoint script.
- Rename a file to `*.toml.disabled` to park a service without deleting it.

### Replicas (PM2 replacement)

`replicas = N` runs N supervised copies of the command; `replicas =
"auto"` tracks the detected CPU count (bounded by `replicas_min` /
`replicas_max`) and rescales live on `docker update --cpus`. Each
replica gets `ZPINIT_REPLICA_INDEX` in its env, is probed for readiness
individually, and shows up as `NAME/N` in zpctl. Listener workloads must
opt into port sharing (Node 22.12.0+: `reusePort: true` in
`server.listen`), otherwise every replica but one crashes with
EADDRINUSE. zpinit also injects `ZPINIT_CPU_COUNT`, `ZPINIT_CPU_QUOTA`,
and `ZPINIT_MEMORY_BYTES` into every child.

## Operate with zpctl (mode 3)

```sh
zpctl status [--verbose] [--json]   # states, per replica
zpctl start|stop|restart NAME | all # --wait blocks until ready
                                    # none of these ever end the container
zpctl reload NAME                   # in-place: reload_signal/_command, else stop+start
zpctl tail [-f] NAME                # last 8KB of a service log; -f streams
zpctl reread                        # dry-run config diff
zpctl update [NAME...]              # apply config changes (same as SIGHUP)
zpctl ready                         # exit 0 iff all services running and ready
zpctl shutdown                      # exit PID 1; ends the container
```

`NAME/N` targets replica N. Editing files under `services/` then
running `zpctl reread` (preview) and `zpctl update` (apply) is the
standard change workflow; a malformed file is skipped with an error
while the other services still load.

## Validate before deploy

Run these in CI or after generating config:

```sh
zpinit --check-config /etc/zpinit/   # TOML + schema validation, exit non-zero on error
zpinit --plan         /etc/zpinit/   # resolved boot plan dry-run (replicas expanded)
zpinit --doctor       /etc/zpinit/   # superset: binaries on PATH, runtime versions, live state
```

## Quick experiment without building an image

```sh
docker run -tid --name zpinit ghcr.io/0ploy/zpinit
docker exec -it zpinit bash
# add TOML under /etc/zpinit/services/, then: zpctl reread && zpctl update
```

The published image runs supervise mode with zero services: the control
socket is up and the container stays alive, so you can iterate on
service files interactively before baking them into your own image.

## Detailed documentation

Read these in the zpinit repo (https://github.com/0ploy/zpinit) when
the task goes beyond this skill:

- `docs/configuration.md`: full TOML schema, env precedence,
  `[resources]` block, validation rules.
- `docs/clustering.md`: replicas, reusePort per runtime (Node, Bun,
  Deno, Python, Go), PM2 comparison and migration.
- `docs/architecture.md`: state machine, boot/reload/shutdown
  internals, control protocol.
- `docs/security.md`: threat model, control socket permissions, env
  injection.
- `docs/why.md`: design decisions and non-goals (no log rotation, no
  dependency graphs, no env interpolation).
