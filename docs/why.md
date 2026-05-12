# Why zpinit

## Why we built this

Every container image we ship eventually grows the same shape:

- A reaper to clean up zombie processes (tini, dumb-init, or hope).
- A `docker-entrypoint.sh` that runs `composer install`, applies
  migrations, fixes permissions, then `exec`s the real CMD.
- supervisord for images that run more than one process: php-fpm +
  nginx, php-fpm + a worker, redis + an app, and so on.

Three tools, three configuration formats, three mental models.
supervisord drags ~30-50 MB of Python into every image and isn't even
real PID 1 (it still wants tini in front). The entrypoint script is
bespoke per image and grows organically until nobody trusts it.

zpinit folds all three into one Go binary. Same supervisor in every
image, same config shape, same operator commands, ~3 MB.

## Design decisions

**CMD wins over services.** When a CMD is provided, zpinit `exec`s it
and ignores `services/` entirely. Same image, three behaviors
(production / debug / task) without flags. We deliberately rejected
adding a separate "supervise + main task" mode; express foreground
tasks as a service with `restart = "never"` plus `exit_code_from`.

**One Wait4 site.** zpinit reaps every child via a single
`wait4(-1, WNOHANG)` loop dispatched per PID; never `cmd.Wait()` per
process. The two race against each other; whichever the kernel
satisfies first wins, the loser gets `ECHILD`, and you lose the exit
code. tini does it the same way.

**Filename ordering, not dependency graphs.** Services start in
lexicographic order; readiness probes block the next start. This is
enough at our scale; adding a real DAG would mean another DSL to learn
for marginal benefit.

**Readiness via a separate probe command.** Rather than parsing log
output or sniffing ports, you tell zpinit how to ask the service if
it's ready (`redis-cli ping`, `mysqladmin ping`,
`curl -f http://127.0.0.1/health`). Most reliable, most explicit.

**FD inheritance for stdout/stderr, never pipes.** Pipes mean we'd have
to drain them. Drain too slowly and the service deadlocks on writes.
Inherit fds: point them at the terminal, at a file, or at whatever the
kernel decides, and the service writes directly without zpinit in the
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
actually bite (zombie leaks, stuck shutdowns, missed orphan reaping,
race conditions in startup ordering) get explicit attention in the
design and tests. Each is the kind of bug that takes a week to surface
and degrades hosts silently.

## Non-goals

Deliberately not in scope. Feature requests matching one of these get
pushed back:

- Log rotation. Use logrotate, or stdout to host logging.
- Log capture via pipes. FD inheritance only; pipes deadlock.
- Service dependency graphs. Filename order + readiness probes only.
- Env interpolation in configs.
- Web UI / XML-RPC / metrics endpoint.
- Windows / FreeBSD support.
- Interactive `zpctl fg`.
