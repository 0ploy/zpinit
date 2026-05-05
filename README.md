# zpinit

A single static Go binary that runs as PID 1 in Docker containers, folding together what
typically takes three tools (tini + a hand-rolled `docker-entrypoint.sh` + supervisord)
into one consistent mental model.

**Status: in development.** Phase 4 of 8 complete. Wrap mode is shippable end-to-end and
already replaces tini for single-process images. The supervisor (multi-service mode) is
under construction.

## How it works

At startup, zpinit always runs `/etc/zpinit/entrypoint.d/*` (executable scripts, lexico-
graphic order). Then it dispatches based on whether the container was given a CMD:

| Setup                                              | Mode      | Behaviour                                              |
| -------------------------------------------------- | --------- | ------------------------------------------------------ |
| no CMD, populated `services/`                      | supervise | runs services as PID 1, manages lifecycle              |
| any CMD (`docker run image bash`, `… php cli …`)   | wrap      | `exec`s the CMD as PID 1, `services/` is ignored       |
| no CMD, empty `services/`                          | error     | exits non-zero with a clear "nothing to do" message    |

This means a single image serves production, interactive debug shells, and one-off tasks
without flags or environment switches:

```sh
docker run myimage                              # production: supervise services
docker run -it myimage bash                     # debug: setup runs, then a shell; services don't start
docker run --rm myimage php bin/console fix:thing   # one-off task with all the setup, no daemons
```

## Configuration

```
/etc/zpinit/
├── zpinit.toml          # globals (all optional)
├── services/            # one TOML per service
│   ├── 10_redis.toml
│   ├── 20_php-fpm.toml
│   └── 99_worker.toml
└── entrypoint.d/        # executable scripts; non-executable ones are skipped
    ├── 10-fix-perms.sh
    └── 20-warmup.sh
```

Filename order determines service start order. Service names default to the filename
without the numeric prefix and `.toml` extension (`10_redis.toml` → `redis`); a TOML
`name = "..."` field overrides if you need a different identity (used by zpctl, logs,
`exit_code_from`).

The full schema is in `internal/config/config.go`. Validate a config without spawning
anything:

```sh
zpinit --check-config /etc/zpinit/
```

Exit 0 with a one-line OK summary, or exit 1 with every problem found in one pass.

## Foreground worker pattern

Some images need a long-running supporting daemon (php-fpm) and a foreground worker
(Symfony `messenger:consume`). Express the worker as a service with `restart = "never"`,
and point `exit_code_from` at it:

```toml
# zpinit.toml
exit_code_from = "worker"
```

```toml
# services/99_worker.toml
name = "worker"
command = ["php", "bin/console", "messenger:consume"]
restart = "never"
```

When the worker exits, zpinit gracefully shuts down the rest of the supervised services
and exits with the worker's exit code.

## Development

```sh
make build                # static binaries to bin/
make test                 # unit tests (Linux + macOS)
make integration          # full-binary integration tests (Linux only)
make lint                 # gofmt + go vet
```

Linux-only in production (uses `Pdeathsig`, `Setpgid`, `/proc`); macOS dev compiles via
build tags but skips PID-1-specific behaviour.

CI runs unit tests on push (Linux + macOS), integration on PRs and pushes to main.

## License

MIT. See `LICENSE`.
