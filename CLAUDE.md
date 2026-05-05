# CLAUDE.md

## About This File
Agent guidance — decisions, conventions, and gotchas that can't be inferred
from reading code. Keep it minimal: every line competes for the agent's
attention and biases its behavior. Don't add file listings, tech stack
descriptions, or anything an agent discovers by exploring the source. When
updating, ask for each line: "would removing this cause an agent to make a
mistake?" If not, cut it.

## Project Overview
zpinit: single static Go binary that runs as PID 1 in ScaleCommerce's Docker
images, replacing supervisord + tini + per-image bespoke `docker-entrypoint.sh`.
Linux-only in production (Pdeathsig, /proc); macOS dev compiles via build
tags but doesn't exercise PID-1 paths.

User-facing docs are in `README.md` (onboarding, examples, high-level design
decisions). Per-phase implementation rationale lives in commit history —
`git log` is authoritative.

## Load-Bearing Design Rules

**CMD wins over services.** When a CMD is supplied (i.e. `flag.Args()`
non-empty after zpinit parses its own flags), zpinit `syscall.Exec`s it as
PID 1 and ignores `services/` entirely. Same image gets three modes from
this rule alone: production (no CMD → supervise), debug
(`docker run image bash` → wrap), one-off task
(`docker run image php cli …` → wrap). Never re-add a "supervise + main
task" combined mode — express foreground workers as a service with
`restart = "never"` plus `exit_code_from = "<worker>"`. Deliberately
rejected during design.

**Reaping uses tini's pattern.** One `wait4(-1, WNOHANG)` site, in
`internal/reaper.Reap`, dispatched by PID to per-service exit channels.
Never call `cmd.Wait()` per service — it races the centralized reap loop;
whichever the kernel satisfies first wins, the loser gets `ECHILD`, the
exit code is lost. `SpawnTracked` holds its mutex across `cmd.Start()` so
the new PID is registered atomically, closing the spawn-then-track race
for fast-dying children.

## Non-Goals (Don't Add These)
No log rotation (use logrotate or stdout → host logging), no log capture
via pipes (FD inheritance only — pipes deadlock), no service dependency
graphs (filename order + readiness probes only), no env interpolation in
configs, no web UI / XML-RPC / metrics endpoint, no Windows / FreeBSD
support, no interactive `zpctl fg`. If a feature request matches one of
these, push back and reconfirm before coding.

## Conventions
- Tests use the standard library only — no testify, gomock, or similar.
- Only approved external dependency: `github.com/BurntSushi/toml`. Anything
  else needs explicit approval before `go get`.
- `MaxConsecutiveCrashes = 5` is hardcoded as the retry budget. Spec says
  "retry budget" without naming a config key; promote to config only if asked.
- If a design decision is ambiguous, ask before coding. The architecture is
  the result of multi-round discussion; re-litigating without surfacing
  burns time and risks shipping a different product than intended.

## Gotchas
- `ZPINIT_ENV_FILE` is an internal/test override for the env-file path.
  Production always reads `/run/zpinit/env`. Don't expose it in `--help` or
  document it publicly.

- `boot_timeout` starts when service-boot begins, not at zpinit launch —
  per-script timeouts cover the entrypoint.d phase separately, so a slow
  `composer install` doesn't eat the service-boot budget.

- Reload-removing the `exit_code_from`-watched service shuts the whole
  supervisor down: its terminal-state watcher fires and triggers shutdown.
  Don't reload-remove the watched service.
