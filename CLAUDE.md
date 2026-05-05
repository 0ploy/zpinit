# CLAUDE.md

## About This File
Agent guidance — decisions, conventions, and gotchas that can't be inferred from reading
code. Keep it minimal: every line competes for the agent's attention and biases its
behavior. Don't add file listings, tech stack descriptions, or anything an agent discovers
by exploring the source. When updating, ask for each line: "would removing this cause an
agent to make a mistake?" If not, cut it.

## Project Overview
zpinit: a single static Go binary that runs as PID 1 in ScaleCommerce's Docker images,
replacing supervisord + tini + per-image bespoke `docker-entrypoint.sh`. Companion CLI is
zpctl. Linux-only in production (uses Pdeathsig, Setpgid, /proc); macOS dev compiles via
build tags but doesn't exercise PID-1 paths.

User-facing docs in `README.md`. Phase plan and design rationale live in commit history
(`git log` is authoritative).

## Load-Bearing Design Rules

**CMD wins over services.** When a CMD is supplied (i.e. `flag.Args()` non-empty after
zpinit parses its own flags), zpinit `syscall.Exec`s the CMD as PID 1 and ignores
`services/` entirely. Same image gets three modes from this rule alone: production
(no CMD → supervise), debug (`docker run image bash` → wrap), one-off task (`docker run
image php cli ...` → wrap). Never re-add a "supervise + foreground task" combined mode —
foreground workers belong in `services/` with `restart = "never"` plus
`exit_code_from = "<worker>"`. The temptation to add a third mode was deliberately
rejected during design.

**Reaping is tini-pattern: one Wait4 site, no per-process cmd.Wait.** They race against
each other when both are present; whichever the kernel satisfies first wins, and the
loser gets ECHILD and we lose the exit code. The reaper exposes `SpawnTracked` that
holds its mutex across `cmd.Start()` so the new PID is registered before any
SIGCHLD-driven dispatch can observe it — closes the Spawn-then-Track race for
fast-dying children.

**`syscall.Kill(-pid, sig)` (negative PID) signals the whole process group.** Single-PID
kill leaves forking daemons (php-fpm master + workers) running. Always use SignalGroup.

## Non-Goals (Don't Add These)
No log rotation, no log capture via pipes (FD inheritance only — pipes deadlock), no
service dependency graphs (filename order + readiness probes only), no env interpolation
in configs, no web UI / XML-RPC / metrics endpoint, no Windows / FreeBSD, no interactive
`zpctl fg`. If a feature request matches one of these, push back and reconfirm before
implementing.

## Implementation Discipline
- Implement phases in order; don't skip ahead. After each phase, `make test`, `make lint`,
  and `make integration` must pass before moving on.
- After completing a phase, update CLAUDE.md and README.md to reflect the new state in
  the same commit as the phase work.
- Tests-first for non-trivial logic: state machine, backoff, reload diff, mode detection.
  Glue code doesn't need this.
- If a design decision is ambiguous, ask before coding. Don't invent options the spec
  didn't list. The architecture is the result of multi-round discussion.

## Conventions That Diverge From Defaults
- Tests use the standard library only — no testify, gomock, or similar.
- Only approved external dependency: `github.com/BurntSushi/toml`. Anything else needs
  explicit approval.
- Process-spawn attributes split by build tag (`sysattr_linux.go` has Pdeathsig;
  `sysattr_other.go` has Setpgid only). Don't try to write portable code that papers
  over the difference.
- `MaxConsecutiveCrashes = 5` in `internal/supervisor` is the retry budget. Spec says
  "retry budget" without naming a config key; promote to config only if asked.
- Commit messages: never add `Co-Authored-By` trailers (per global CLAUDE.md).

## Gotchas
- After `Spawn()`, close the parent's `*os.File` copies of log targets — kernel duplicates
  fds for the child, so the parent's copies leak file handles.
- `ZPINIT_ENV_FILE` is an internal/test override for the env-file path. Production
  always reads `/run/zpinit/env`. Don't expose it in `--help` or document it publicly.
- Phase 4's Stop sends one signal and parks in Stopping until the process exits.
  SIGKILL escalation arrives in Phase 6 — until then, a process that ignores
  `stop_signal` hangs the runner indefinitely.
- `boot_timeout` clock starts after `entrypoint.d` completes, not at zpinit launch —
  per-script timeouts already cover the entrypoint phase, and a slow `composer install`
  shouldn't eat the service-boot budget.
