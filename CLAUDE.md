# CLAUDE.md

## About This File
Agent guidance: decisions, conventions, and gotchas that can't be inferred
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
decisions). Per-phase implementation rationale lives in commit history;
`git log` is authoritative.

## Load-Bearing Design Rules

**CMD wins over services.** When a CMD is supplied (i.e. `flag.Args()`
non-empty after zpinit parses its own flags), zpinit `syscall.Exec`s it as
PID 1 and ignores `services/` entirely. Same image gets three modes from
this rule alone: production (no CMD → supervise), debug
(`docker run image bash` → wrap), one-off task
(`docker run image php cli …` → wrap). Never re-add a "supervise + main
task" combined mode; express foreground workers as a service with
`restart = "never"` plus `exit_code_from = "<worker>"`. Deliberately
rejected during design.

**Reaping uses tini's pattern.** One `wait4(-1, WNOHANG)` site, in
`internal/reaper.Reap`, dispatched by PID to per-service exit channels.
Never call `cmd.Wait()` per service: it races the centralized reap loop;
whichever the kernel satisfies first wins, the loser gets `ECHILD`, the
exit code is lost. `SpawnTracked` holds its mutex across `cmd.Start()` so
the new PID is registered atomically, closing the spawn-then-track race
for fast-dying children.

## Non-Goals (Don't Add These)
No log rotation (use logrotate or stdout → host logging), no log capture
via pipes (FD inheritance only; pipes deadlock), no service dependency
graphs (filename order + readiness probes only), no env interpolation in
configs, no web UI / XML-RPC / metrics endpoint, no Windows / FreeBSD
support, no interactive `zpctl fg`. If a feature request matches one of
these, push back and reconfirm before coding.

## Conventions
- No em-dashes (—) in any writing. Use periods, colons, or semicolons.
- Tests use the standard library only. No testify, gomock, or similar.
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

- `boot_timeout` starts when service-boot begins, not at zpinit launch.
  Per-script timeouts cover the entrypoint.d phase separately, so a slow
  `composer install` doesn't eat the service-boot budget.

- Reload-removing the `exit_code_from`-watched service shuts the whole
  supervisor down: its terminal-state watcher fires and triggers shutdown.
  Don't reload-remove the watched service.

- Production code calls `Runner.StartCtx`/`StopCtx`, never bare
  `Start`/`Stop`. The bare versions block forever if the Run goroutine has
  exited (cmds buffer accepts the send, but `<-done` never fires). Bare
  versions exist only for tests where Run is always alive.

- `Orchestrator.runners` and `.cfg` are protected by `o.mu` (RWMutex);
  Reload-vs-Reload is serialized by `o.reloadMu`. External readers (control
  server) must use `snapshotRunners()`: iterating the live slice while
  Reload mutates it is a data race confirmed by `go test -race`.

- `Reload` registers added/restart-new runners synchronously but boots
  them in a single detached goroutine (`runReloadBoots`), one at a time
  in filename order. Don't make boot synchronous (sum of boot_timeouts
  blew past any reasonable client deadline) and don't fan out one boot
  goroutine per service (loses the readiness-blocks-next-service
  property that initial boot relies on). Boots use `o.runnerCtx`, not
  the reload caller's ctx, so they survive client disconnect.

- `Reload` propagates `globals.Env` changes by adding every reloadable
  service to the restart list (children can't be re-env'd in place).
  main.go installs `Orchestrator.SetBaseEnvBuilder` to recompose
  baseEnv from new globals + boot-time `containerEnv` + boot-time
  `scriptEnv` (entrypoint.d delta captured in `run`). Without a
  builder, `Reload` leaves `baseEnv` unchanged (the test default).

- `removeService` only deregisters the runner after `WaitTerminal`
  succeeds. On stop failure it returns an error and leaves the runner
  registered so its `Run` goroutine keeps tracking the still-live
  child; the next reload sees it in the diff and retries. Dropping a
  runner with a live child would leak an unmanaged process under PID 1
  with no zpctl handle. `Reload` joins these errors and returns them;
  `runCancel` is only called on successful removal so the Run loop
  stays alive while the child is still alive.

- `stopAll` signals **and** waits one service at a time, in reverse
  filename order. Filename order encodes dependency order during boot,
  so reverse-serial teardown ensures dependents fully drain through
  their dependencies before the dependency itself receives SIGTERM.
  The previous "signal reverse, wait parallel" version was faster but
  could break flush-on-shutdown semantics. Per-service SIGKILL
  escalation bounds any one stuck service.

- Shutdown wait budget is recomputed at signal time via
  `Orchestrator.ShutdownBudget()`, not snapshotted at boot. Reload can
  add services or bump `stop_timeout` after launch, and the supervisor
  outer wait must always cover stopAll's serial inner wait, otherwise
  the runtime hard-kills PID 1 mid-graceful-shutdown.
