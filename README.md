# zpinit

A single static Go binary that runs as PID 1 in your Docker container.
The same binary covers all three use cases:

1. [**Single Process Mode**](#single-process-mode) (replaces **tini**)
2. [**Setup, then Run Mode**](#setup-then-run-mode) (replaces **`docker-entrypoint.sh`**)
3. [**Manage Services Mode**](#manage-services-mode) (replaces **supervisord**)

![zpinit: run containers the way you want. Three Docker use cases, one binary.](docs/modes.png)

## Try it out in Manage Service Mode

```sh
docker run -tid --name zpinit ghcr.io/0ploy/zpinit
```

The published image runs zpinit as PID 1 with no services. The control
socket is up, `zpctl` works, and the container stays alive. Now install 
and configure nginx:
```sh
docker exec -it zpinit bash

apk add --no-cache nginx

cat > /etc/zpinit/services/10_nginx.toml <<'EOF'
command = ["/usr/sbin/nginx", "-g", "daemon off;"]
restart = "always"
EOF

zpctl reread       # diff preview
zpctl update       # apply
zpctl status

curl -I http://localhost
```

Same workflow for every other mode below: bake the config into your own
image once you've got the service files you want.

## 1. Single Process Mode

**When to use it.** Your image runs one well-behaved workload (a Go
server, a long-running CLI) and you don't need any setup
work before it starts.

**What zpinit does.** Validates config, then `syscall.Exec`s your CMD.
The CMD takes over as PID 1; zpinit is gone after the exec. (If your
workload doesn't reap its own children and you need that, use [Manage
Services Mode](#manage-services-mode) with one entry instead. Then
zpinit stays as PID 1 and reaps for you.)

**Why zpinit here?** You want to inject Environment Variables into the CMD 
instead of the whole Container. Via `[env]` in `zpinit.toml` these variables 
will not been seen by `docker exec` but are available for the CMD itself.

Example Dockerfile:
```dockerfile
FROM alpine:latest

COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpinit /usr/local/bin/
COPY zpinit.toml /etc/zpinit/zpinit.toml
COPY my-app /usr/local/bin/

ENTRYPOINT ["zpinit"]
CMD ["my-app", "--port", "8080"]
```
Example `[env]` section in `/etc/zpinit/zpinit.toml`:
```toml
[env]
API_KEY   = "xxxxxxxxxxxxx"
LOG_LEVEL = "info"
```
See [docs/configuration.md](docs/configuration.md) and [docs/security.md](docs/security.md) for details about this.

## 2. Setup, then Run Mode

**When to use it.** You need preparation work before the workload
starts: render config from env, apply migrations, fix permissions, warm
a cache. Today this lives in a `docker-entrypoint.sh` that ends with
`exec "$@"`. zpinit replaces that script.

**What zpinit does.** Runs every executable (or script) in `/etc/zpinit/entrypoint.d/`
in lexicographic order, drains any zombies they leave behind, then
`syscall.Exec`s your CMD. A non-zero exit from a script aborts the
container (or continues, with `entrypoint_on_failure = "continue"`).

**Why zpinit here?** A hand-rolled docker-entrypoint.sh reinvents the same plumbing in every image: `set -e`,
signal traps, `exec "$@"`, ad-hoc timeouts etc. zpinit replaces that file with a
directory of small executables in any language. Each script gets a per-step timeout (a stuck `composer
install` hits its own deadline instead of hanging container boot), zombies left behind by sub-shells are
drained between steps, and `entrypoint_on_failure = "continue"` marks a step non-critical without
re-implementing skip logic per script. The `[env]` injection from mode 1 still applies, plus scripts can
write to `/run/zpinit/env` to hand variables forward to the next script or the CMD.

Example Dockerfile:
```dockerfile
FROM node:20-alpine

WORKDIR /app

COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpinit /usr/local/bin/
COPY entrypoint.d/ /etc/zpinit/entrypoint.d/

COPY package.json package-lock.json ./
RUN npm ci --omit=dev

COPY dist/    ./dist/
COPY drizzle/ ./drizzle/

ENTRYPOINT ["zpinit"]
CMD ["node", "./dist/server.js"]
```

Example `entrypoint.d/10-drizzle-migrate.sh`:
```bash
#!/bin/sh
set -eu

: "${DATABASE_URL:?DATABASE_URL must be set}"

echo "applying drizzle migrations..."
node ./dist/migrate.js
echo "migrations done."
```
A setup script is just an executable. Any language with a shebang works. Files have to be executable. Non-executable files are skipped at
runtime and surfaced as a warning by `--check-config`. Files starting
with `.` or ending in `.disabled` are ignored.

## 3. Manage Services Mode

**When to use it.** Your image runs multiple processes. Today this is supervisord plus tini in front.

**What zpinit does.** Reads `/etc/zpinit/services/*.toml`, starts each
service in filename order, and uses readiness probes to gate the next
service's start. zpinit stays around as PID 1: it reaps, restarts on
crash with backoff, applies config reloads, and handles graceful
shutdown.

**Why zpinit here?** supervisord drags 80 MB of Python into every image and isn't real PID 1; it still
wants tini in front to reap orphans. zpinit is one ~3 MB Go binary that does both jobs, with the same
TOML schema you saw in modes 1 and 2 — one mental model across every image in the fleet. Boot ordering
is declarative (filename order + readiness probes gate the next service's start), no priority plus
pre-start sleeps to fake it.

Example Dokcerfile:
```dockerfile
FROM ubuntu:24.04

RUN apt-get update \
  && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      nginx \
      php8.4-fpm \
  && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpinit /usr/local/bin/
COPY --from=ghcr.io/0ploy/zpinit:latest /usr/local/bin/zpctl  /usr/local/bin/

COPY services/ /etc/zpinit/services/

EXPOSE 80
ENTRYPOINT ["zpinit"]
# No CMD: supervise mode.
```
The Dockerfile has **no `CMD`**. CMD wins over services, so adding one
puts you back in wrap mode and `services/` is ignored.

Exmaple `services/10_php-fpm.toml`:
```toml
command = ["/usr/sbin/php-fpm8.3", "-F"]
restart = "always"

[ready]
command  = ["sh", "-c", "test -S /run/php/php8.3-fpm.sock"]
interval = "200ms"
timeout  = "10s"

[log]
stdout = "/var/log/zpinit/php-fpm.out.log"
stderr = "/var/log/zpinit/php-fpm.err.log"
```
`[ready]` is an optional probe block on a service. zpinit runs the command on a loop every `interval` for a maximm time of `timeout` after starting the
service and treats the service as "ready" the first time it exits 0. The next service in filename order
doesn't start until then. If `[ready]` is omitted, the service is ready the instant it spawns (no gating).

`[log]` redirects the service's stdout/stderr fds at spawn time. `inherit` (the default, used by nginx
above) hands them to the container's own stdout/stderr - what your docker/k8s log collector wants. A
path opens the file with `O_APPEND|O_NOFOLLOW` and the service writes directly, no pipe in zpinit's data
path. File-logged services are inspectable live with `zpctl tail php-fpm`.

Example `services/20_nginx.toml`
```toml
command = ["/usr/sbin/nginx", "-g", "daemon off;"]
restart = "always"
```

**Reload without restart.** `SIGHUP` (or `zpctl update`) re-reads
`/etc/zpinit/`, diffs against the running set, and applies:

- New file: start the new service.
- Removed file: graceful stop.
- Changed content: restart (unless `reloadable = false`).
- Renamed file: remove + add.

`zpctl reread` previews the diff without applying.

**Validate before deploying.**

```sh
zpinit --check-config /etc/zpinit/
```

Loads everything, applies defaults, validates, and either prints an OK
summary or every error in one pass. Exit 0 / 1.

**Operator commands.** `zpctl` talks to zpinit over `/run/zpinit.sock`.
The socket is bound `0600` and gated by `SO_PEERCRED`: only processes
running as the daemon's UID (root in a normal container) can issue
commands. Non-root services in the same container cannot use zpctl.
State names match supervisorctl exactly so existing muscle memory
transfers.

```sh
zpctl status [service]           # all services, or one
zpctl start | stop | restart [svc]
zpctl signal redis HUP           # nginx-style "reload your own config"
zpctl pid [service]              # zpinit's PID, or the service's
zpctl tail redis                 # snapshot of file-logged stdout
zpctl update                     # apply config changes (= SIGHUP)
zpctl reread                     # dry-run config diff
zpctl shutdown
zpctl help
```

## Learn more

- [docs/why.md](docs/why.md): why we built this, design decisions, philosophy, non-goals.
- [docs/configuration.md](docs/configuration.md): full config schema (env, cwd, user/group, logs, backoff, stop signals, defaults).
- [docs/architecture.md](docs/architecture.md): packages, state machine, internals.
- [docs/security.md](docs/security.md): security considerations.
- [docs/development.md](docs/development.md): build, test, contribute.

## License

MIT. See `LICENSE`.
