# Node.js Clustering (replicas + reusePort)

zpinit's `replicas = N` on any service spawns N first-class supervised
copies of the same command — each with its own PID, log file, crash
budget, and `zpctl` row. For listener workloads, the app opts into
kernel-level port sharing via `reusePort: true` and the kernel
load-balances incoming connections across the replicas. No master
process, no IPC, no shared listener.

This replaces what PM2 cluster mode does, without the 30+ MB
Node-on-Node daemon.

## Minimal example

`services/30_api.toml`:

```toml
command = ["node", "/app/server.js"]
replicas = 4
restart = "always"

[log]
stdout = "/var/log/api.log"            # all 4 replicas share this file
# stdout = "/var/log/api-{index}.log"  # opt-in: per-replica api-0.log..api-3.log
```

`/app/server.js`:

```js
const http = require('node:http');
const server = http.createServer(handler);
server.listen({ port: 3000, reusePort: true });
```

Each replica gets `ZPINIT_REPLICA_INDEX=0..N-1` in its env. By default
all replicas write to the same log file (Linux `O_APPEND` is atomic
for line-sized writes, so they don't tear). Use `{index}` in the path
for per-replica files when you need them, or have the app prefix each
line with `ZPINIT_REPLICA_INDEX` for attribution in the shared file.

## Listener support by runtime

Listener replicas need the runtime to expose `SO_REUSEPORT`. Non-listener
replicas (queue consumers, workers, cron-style jobs) work on any
runtime.

**Node.js 22.12.0 or newer.** `reusePort` on `net.Server.listen` was
added in 22.12.0 LTS (December 3, 2024, PR #55408). Older versions
silently ignore the option; only the first replica binds, the rest get
`EADDRINUSE`.

```js
server.listen({ port: 3000, reusePort: true });
```

**Bun (any 1.x).** Available since Bun 1.0.

```js
Bun.serve({ port: 3000, reusePort: true, fetch: handler });
```

**Deno (any modern).**

```js
Deno.serve({ port: 3000, reusePort: true }, handler);
```

**Python.** Frameworks expose it (`uvicorn --reuse-port`,
`gunicorn --reuse-port`). Plain `http.server` needs
`socket.setsockopt(SOL_SOCKET, SO_REUSEPORT, 1)` before bind.

**Go.** Use `net.ListenConfig.Control` to set `SO_REUSEPORT` on the
listening fd before bind — no `LD_PRELOAD` shim for static Go binaries.

## Compared to PM2 cluster mode

| property                            | PM2 cluster mode             | zpinit replicas + reusePort       |
| ----------------------------------- | ---------------------------- | --------------------------------- |
| N workers across cores              | yes                          | yes                               |
| auto-restart per worker             | yes                          | yes (each is a first-class child) |
| per-worker logs                     | yes                          | yes (`{index}` template or `.N`)  |
| graceful shutdown                   | yes                          | yes (existing `stop_timeout` path) |
| zero-drop rolling reload            | yes (master holds listener)  | best-effort (per-replica drain)   |
| `cluster.worker.id`, `process.send` | yes                          | no (Node cluster IPC unavailable) |
| PID-1 correctness                   | weak (PM2 is a Node app)     | strong (static Go binary)         |
| security hardening                  | none                         | peer-cred, O_NOFOLLOW, umask      |

The load-bearing trade is rolling-reload semantics: PM2's master holds
the listener and hands off, so deploys never drop a connection.
zpinit's per-replica accept queue can drop on close. For most workloads
where SIGTERM drain handles in-flight requests gracefully, the
difference is occasional reset connections during deploys. If that's a
hard SLA, the answer is an upstream load balancer that drains a replica
before zpinit signals it — not a zpinit feature.

Replicas of an app that binds a port without `reusePort` support will
silently fail with `EADDRINUSE` on every replica except the first that
wins the bind race. `zpinit --doctor` catches the common case (Node
below 22.12.0 with `replicas > 1`) pre-boot. Apps that depend on
`cluster.worker.id` or `process.send` IPC need a refactor before
running under zpinit replicas.

## Migrating from PM2

A PM2 deployment is typically one `ecosystem.config.js` file. The
analog in zpinit is one TOML file per service:

```toml
# services/30_api.toml
command = ["node", "/app/server.js"]
replicas = 4            # was: instances: 4
restart = "always"      # was: autorestart: true
```

Drop PM2 from the image, copy in the zpinit binary, and the image that
was `CMD ["pm2-runtime", "start", "ecosystem.config.js"]` becomes
`ENTRYPOINT ["zpinit"]` with no CMD — manage-services mode in the
[README](../README.md).

## Operator commands for replicated services

`zpctl` accepts both bare service names and `svc/N`:

```sh
zpctl status                # shows api/0, api/1, api/2, api/3
zpctl restart api/2         # restart just replica 2
zpctl tail api/0            # snapshot replica 0's log
zpctl signal api TERM       # signal all replicas
```

Commands that target exactly one process — `pid`, `tail` — error out
for bare replicated names (ambiguous). Use `svc/N`.

## How it works under the hood

`replicas = N` is implemented in the orchestrator's runner-expansion
layer: one TOML file produces N first-class `Runner` instances, each
with a distinct `replicaIndex` but sharing one `Spec` (the on-disk
TOML). Reload-diff is keyed by filename, so a config change restarts
all replicas of a service atomically. Log-path rewriting happens at
runner construction; the orchestrator's reload comparison uses the
unmodified spec so per-replica path rewriting doesn't produce phantom
diffs.

For the design rationale of "no Node-side cluster harness," see
[architecture.md § Replicas](architecture.md).
