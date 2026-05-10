# Changelog

## v0.1.0

Initial release.

### Three-mode supervisor in one static binary

zpinit covers what supervisord, tini, and per-image `docker-entrypoint.sh` do today, with one mental model:

- **Single-process mode.** No `services/` directory and a `CMD` provided. zpinit validates config, then `syscall.Exec`s the CMD. zpinit is gone after the exec; the CMD becomes PID 1.
- **Setup-then-run mode.** Same as above, plus an `entrypoint.d/` directory of executables run in lexicographic order before the exec. Per-script timeouts, zombies drained between steps, optional `entrypoint_on_failure = "continue"` for non-critical steps. Scripts can write to `/run/zpinit/env` to hand variables forward to the next script or the CMD.
- **Manage-services mode.** No `CMD`. zpinit reads `/etc/zpinit/services/*.toml`, starts each service in filename order with optional readiness probes gating the next, supervises restarts with backoff, and stays around as PID 1.

The mode is decided by whether a `CMD` is supplied; one image gets all three uses (production, debug shell, one-off task) without rebuilds.

### PID-1 essentials

- Single `wait4(-1, WNOHANG)` reaper loop dispatched by PID to per-service exit channels (tini's pattern); fast-dying child registration is race-free via spawn-tracked mutex.
- Signal handling forwards SIGTERM/SIGINT to children, SIGHUP triggers config reload, signals are coalesced and serialized.
- Graceful shutdown stops services serially in reverse filename order with per-service SIGKILL escalation; the supervisor's wait budget is recomputed at signal time so reload-added services or bumped `stop_timeout`s are always covered.

### Configuration

- TOML schema for `globals`, `defaults`, per-service `services/*.toml`. No env interpolation, no priority numbers, no dependency graphs: ordering is filename order plus readiness.
- `--check-config` validates an entire config tree in one pass and prints all errors at once. Exit 0/1.
- `[env]` section injects variables into the CMD or service without polluting the container env (invisible to `docker exec`).
- `[globals.env]` table provides shared env applied to all services; reloadable.
- Per-service `[ready]` probe (command + interval + timeout). The next service in filename order does not start until the probe exits 0.
- Per-service `[log]` redirects stdout/stderr at spawn time. `inherit` (default) hands FDs to the container's own stdout/stderr; a path opens with `O_APPEND|O_NOFOLLOW` for direct writes (no pipe in zpinit's data path).

### Reload without restart

`SIGHUP` (or `zpctl update`) re-reads the config tree, diffs against the running set, and applies in filename order:

- New file: start.
- Removed file: graceful stop.
- Changed content: restart (unless `reloadable = false`).
- Renamed file: remove + add.
- Changed `[globals.env]`: every reloadable service is added to the restart list.

`zpctl reread` previews the diff without applying.

### `zpctl` operator client

Talks to zpinit over `/run/zpinit.sock`. State names match supervisorctl exactly so existing muscle memory transfers.

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

- Control socket bound under `umask 0o077`, then `chmod 0600`. No window where the socket exists with looser perms.
- Every accepted connection gated by `SO_PEERCRED`: peer UID must equal the daemon's effective UID. Non-root processes in the same container cannot use `zpctl` even with filesystem access.
- Service log files open with `O_NOFOLLOW`; symlinked leaves rejected. A planted symlink at the configured log path cannot redirect writes into `/etc/shadow` or similar.
- Wire-protocol responses sanitize CR/LF and lone-`.` lines so service-controlled log content cannot split frames or end the body early at the client.

### Build, release, and distribution

- Linux-only static binaries: `zpinit` and `zpctl`, amd64 and arm64.
- Multiarch container image at `ghcr.io/0ploy/zpinit` tagged `:latest`, `:vX.Y.Z`, `:vX.Y`. Alpine-based, no `ENTRYPOINT`: usable for `COPY --from=…` *and* for `docker run --rm -it … sh` to test the tool against a sample config.
- `CI` workflow (`go test`, `go vet`, `gofmt`, integration tests, and a `make build` version-string smoke test).
- `Release` workflow on `v*` tags: builds binaries, generates `checksums.txt`, builds and pushes the multiarch image, attaches binaries + checksums to the GitHub release, and assembles the release body from this file's latest section.
- All GitHub Actions in both workflows pinned to commit SHAs (not tags) so a compromised upstream cannot inject code into our pipeline.
