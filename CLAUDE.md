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
images. Replaces supervisord, tini, the per-image `docker-entrypoint.sh`,
and (in the Node clustering case) PM2. Linux-only in production: Pdeathsig
and /proc are load-bearing; macOS dev compiles via build tags but doesn't
exercise PID-1 paths. Phase-by-phase rationale lives in `git log`.

## Docs landscape
Read the matching doc before touching the topic; update it in the same
commit when the topic changes.

- `README.md`: three-modes overview, quickstart, operator-command summary.
- `docs/why.md`: motivation, design decisions, non-goals.
- `docs/configuration.md`: full TOML schema, `--check-config` rules,
  service-file conventions (dotfile/`.disabled` skip).
- `docs/clustering.md`: replicas, reusePort, PM2 comparison, migration.
- `docs/architecture.md`: packages, state machine, boot/reload/shutdown,
  reaping, control protocol (including the streaming extension).
- `docs/security.md`: threat model, control socket, log handling, env
  injection.
- `docs/development.md`: build/test/lint commands and platform notes.

## Load-Bearing Design Rules

**CMD wins over services.** A non-empty CMD (after `flag.Args()`) makes
zpinit `syscall.Exec` it and ignore `services/`. No CMD means supervise
mode, even with an empty `services/`: the control socket comes up and an
operator can add services via `zpctl reread` or SIGHUP. Same image yields
three modes from this rule: production (no CMD, supervise), debug
(`docker run image bash`, wrap), one-off task (`docker run image php cli`,
wrap). Never re-add a "supervise + main task" combined mode; express
foreground workers as a service with `restart = "never"` plus
`exit_code_from = "<worker>"`. Deliberately rejected during design.

**Reaping uses tini's pattern.** One `wait4(-1, WNOHANG)` site, in
`internal/reaper.Reap`, dispatched by PID. Never call `cmd.Wait()` per
service: it races the centralized reap loop and the kernel picks a winner,
losing the exit code. `SpawnTracked` holds its mutex across `cmd.Start()`
so the new PID is registered atomically, closing the spawn-then-track race
for fast-dying children.

**Control socket has two layered access gates.** Filesystem: bind runs
under `syscall.Umask(0o077)` so the socket is born `0700`, then explicit
`chmod 0600`. Without the umask flip the socket is briefly `0755` and a
non-root local process wins a TOCTOU race. Peer credentials: every
accepted connection is gated by `SO_PEERCRED` (`authorizePeer` in
`peercred_linux.go`), rejected unless the peer UID equals the daemon's
effective UID. Linux only; the macOS dev stub returns nil. If you loosen
socket perms to allow non-root operators, lift the peer-cred check rather
than widen the chmod.

## Non-Goals (push back, don't code)
No log rotation (use logrotate or stdout-to-host-logging), no log capture
via pipes (FD inheritance only; pipes deadlock), no service dependency
graphs (filename order + readiness probes only), no env interpolation in
configs, no web UI / XML-RPC / metrics endpoint, no Windows / FreeBSD
support, no interactive `zpctl fg`.

## Conventions
- **Docs sync.** Every user-visible behavior change updates the matching
  doc(s) in the same commit, plus a `CHANGELOG.md ## Unreleased` entry
  for anything a release note would mention. Stale docs are worse than
  missing ones; the release workflow ships whatever is on disk.
- **No em-dashes** in any writing (CLAUDE.md, code comments, CHANGELOG,
  docs). Use periods, colons, or semicolons.
- Tests use the standard library only. No testify, gomock, or similar.
- Only approved external dependency: `github.com/BurntSushi/toml`.
  Anything else needs explicit approval before `go get`.
- If a design decision is ambiguous, ask before coding. The architecture
  is the result of multi-round discussion; re-litigating without surfacing
  risks shipping a different product than intended.
- **Don't commit, push, or tag on your own.** When a change looks ready,
  summarize what's staged and ask. Operators often try things in a real
  Docker workload before a commit lands; tag pushes trigger a public
  release.
- GitHub Actions in both workflows are pinned to commit SHAs, never
  floating tags (the release pipeline has GHCR push rights). When
  upgrading, dereference the new tag with
  `gh api repos/<owner>/<repo>/git/refs/tags/<tag> --jq '.object.sha'`
  and put the human-readable version in the trailing comment.

## CHANGELOG.md

The release workflow's awk script extracts the latest `## vX.Y.Z`
section verbatim into the GitHub release body (everything between the
first `## ` heading, skipped, and the second `## `, end). So:

- New version sections go at the very top.
- Nothing above the top `## ` heading leaks into the release body.
- The `## Unreleased` heading is the accumulation target between
  releases.

Entries lead with a **bold headline** stating what changed (or broke,
for fixes) followed by 2-4 lines on *what it does now* and *why the
user cares*. No implementation details (file/lock/function names)
unless load-bearing for an operator. No marketing language, no "this
commit", no phase numbers. Internal refactors and CI tweaks live in
`git log`, not here.

**Cutting a release** (when asked):

1. Confirm clean tree + CI green on `main`.
2. Rename `## Unreleased` to `## vX.Y.Z`. Bump choice: breaking
   change is major, new feature is minor, otherwise patch. Pre-1.0
   we may still bump minor for breaking changes; confirm if
   ambiguous.
3. Commit `release: vX.Y.Z`.
4. `git tag vX.Y.Z && git push origin main vX.Y.Z`. Tag push triggers
   the public release; never do this without an explicit go-ahead.

## Gotchas

- `ZPINIT_ENV_FILE` is an internal/test override for the env-file path.
  Production always reads `/run/zpinit/env`. Don't expose it in
  `--help` or document it publicly.

- `boot_timeout` is per-service, both at initial boot and reload: each
  service gets its own fresh `context.WithTimeout`. A legitimately slow
  first service can't starve later services of their probe window.
  Don't refactor `boot` to share one ctx across services. Per-script
  timeouts cover entrypoint.d separately, so a slow `composer install`
  doesn't eat the service-boot budget.

- Reload-removing the `exit_code_from`-watched service shuts the whole
  supervisor down: its terminal-state watcher fires and triggers
  shutdown. Don't reload-remove the watched service.

- Production code calls `Runner.StartCtx`/`StopCtx`, never bare
  `Start`/`Stop`. The bare versions block forever if the Run goroutine
  has exited (`cmds` buffer accepts the send, but `<-done` never
  fires). Bare versions exist only for tests where Run is always alive.

- **Orchestrator lock discipline.** `o.runners`, `.cfg`, `.baseEnv`,
  `.runnerCtx`, `.wg`, `.earlyShutdownCh`, `.shutdownOnce`,
  `.watcherCancel`, and `.watcherGen` are protected by `o.mu`
  (RWMutex). `o.reloadMu` serializes Reload-vs-Reload AND
  Reload-vs-watcher-driven autoscale: `OnResourceChange` holds it
  across the `SetResourceEnv → SetCurrentSnapshot → scaleAutoServices`
  triad so a SIGHUP racing with a watcher commit can't observe a
  half-updated `o.cfg` or overwrite the scaler's `Replicas.N` with a
  stale disk-loaded value. The fanout reload runs outside `reloadMu`
  because per-runner reloads only touch that runner's state. External
  readers must use `snapshotRunners()`; iterating the live slice while
  Reload mutates it is a data race confirmed by `go test -race`.

- Boot paths that need a runner's current baseEnv (readiness probe env
  in `bootOne` / `bootReloadJob`) MUST go through `r.BaseEnv()`, not a
  bare `r.baseEnv` read. `Runner.SetBaseEnv` fires from
  `SetResourceEnv` while initial boot or reload-boot is still running,
  so the bare read races the slice header.

- `Reload` registers added/restart-new runners synchronously but boots
  them in a single detached goroutine (`runReloadBoots`), one at a time
  in filename order. Don't make boot synchronous (sum of boot_timeouts
  blew past any reasonable client deadline) and don't fan out one boot
  goroutine per service (loses the readiness-blocks-next-service
  property that initial boot relies on). Boots use `o.runnerCtx`, not
  the reload caller's ctx, so they survive client disconnect.
  `runReloadBoots` holds `o.reloadBootMu` for its loop so back-to-back
  reloads serialize their boot phases; `reloadBootMu` is separate from
  `reloadMu` so the diff phase of reload N+1 isn't blocked by the boot
  phase of N (which can run for many seconds).

- `Reload` propagates `globals.Env` changes by restarting every
  reloadable service (children can't be re-env'd in place). main.go
  installs `SetBaseEnvBuilder` to recompose baseEnv from new globals +
  boot-time `containerEnv` + boot-time `scriptEnv` (entrypoint.d delta
  captured in `run`). Without a builder, `Reload` leaves `baseEnv`
  unchanged (the test default).

- `removeServiceGroup` only deregisters runners after `WaitTerminal`
  succeeds. On stop failure it leaves the runner registered so its Run
  goroutine keeps tracking the still-live child; the next reload diff
  retries. Dropping a runner with a live child would leak an unmanaged
  process under PID 1 with no zpctl handle. `runCancel` is called only
  on successful removal so the Run loop stays alive while the child
  is.

- `stopAll` schedules teardown per filename group: reverse filename
  order BETWEEN groups (filename encodes dependency order so dependents
  drain through their dependencies before SIGTERM), parallel WITHIN a
  group (all replicas share a logical service and have no inter-replica
  flush ordering). `removeServiceGroup` reuses the same shape on the
  reload path. The control-socket verbs `start` / `stop` / `restart`
  follow it too, so `zpctl restart all` on `replicas = 64` finishes in
  one stop_timeout instead of 64.

- Shutdown wait budget is recomputed at signal time via
  `Orchestrator.ShutdownBudget()`, never snapshotted at boot. Counts
  one `(stop_timeout + reapGrace)` per filename group, not per runner.
  Reload can add services or bump `stop_timeout` after launch; the
  supervisor outer wait must cover stopAll's inner wait, otherwise the
  runtime hard-kills PID 1 mid-graceful-shutdown.

- Service log files (`log.stdout`/`log.stderr`), `cmdTail` reads, and
  `cmdTailFollow` reopens (on rotation) all open with `O_NOFOLLOW`. A
  symlink at the leaf is rejected; `readLastBytes` and the follow loop
  also require `Mode().IsRegular()`. The parent directory is auto-
  created with `MkdirAll` mode 0755 just before the open, so operators
  don't need a per-image `entrypoint.d/00-mklogdir.sh`. Only operator-
  named paths are ever mkdir'd; the leaf check is unaffected.

- Wire-protocol responses sanitize every line via `sanitizeLine`:
  CR/LF become spaces and a lone `.` is prefixed with a space. The
  one-shot path (`WriteResponse`) and the streaming helpers
  (`WriteStatusLine` / `WriteBodyLine` / `WriteEnd`, used by
  `tail --follow`) share the same sanitizer. Don't add a streaming
  write path that bypasses it: log content and TOML errors can carry
  CR/LF, and a tainted line would otherwise split fields across
  frames or end the body early at the client.

- Streaming-verb dispatch (`isStreamingRequest` → `handleStream`) runs
  the handler with the read deadline cleared and a per-drain write
  deadline. `handleStream` writes the response terminator after the
  handler returns; handlers must NEVER write the terminator
  themselves. New streaming verbs add to `isStreamingRequest`, write
  a status line, stream body lines via `pc.WriteBodyLine`, return.

- `replicas = N` expands one service spec into N first-class Runners
  with the same `cfg.Filename` but distinct `replicaIndex`. The diff
  key stays at the filename level. Service-spec comparison uses
  `r.Spec()` (the unmodified spec), not `r.Cfg()`, because the
  per-replica `Log.Stdout` rewrite (`{index}` expansion) would
  otherwise show as a phantom diff every reload. Use
  `NewRunnerForReplica(cfg, spec, ...)` (via `expandServiceToRunners`
  or `scaleUp`) to construct per-replica runners; never set `r.spec`
  by hand.

- Replica log paths default to a shared file across all N replicas.
  `replicaLogPath` only rewrites when the spec contains `{index}`.
  Linux `O_APPEND` is atomic below `PIPE_BUF` (typically 4096 bytes),
  so concurrent appends from N replicas don't tear line-sized log
  output. Operators wanting per-replica files opt in via `{index}`.

- `findRunnerLocked(name)` matches by `cfg.Name` only and returns the
  first replica for replicated services. Control verbs use
  `resolveTarget(snap, arg)` which parses `svc/N`. `exit_code_from` is
  rejected at config-load for replicated/auto services so its
  `findRunnerLocked` call site stays unambiguous.

- The `exit_code_from` watcher uses a per-installation generation
  counter (`watcherGen`). Each `installExitCodeWatcher` bumps it; the
  spawned goroutine captures it and re-checks under `o.mu.RLock`
  before `once.Do(close(earlyCh))`. Cancel of `wctx` doesn't
  synchronize with goroutine progress, so the gen check is required:
  a retarget racing with the old target reaching terminal state would
  otherwise shut the supervisor down for a service the new config no
  longer cares about.

- Bounded post-SIGKILL reap waits in `entrypoint.runOne` and
  `probe.defaultProber` (constants `scriptReapGiveUp` /
  `probeReapGiveUp`, both 5s). A child pinned in uninterruptible
  kernel sleep (D state) can't be SIGKILL'd until its syscall
  completes. The cap=1 buffered channel each helper waits on absorbs
  the eventual reap send, so abandoning is safe (one leaked goroutine,
  no memory growth).

- Backoff carries per-replica deterministic ±10% jitter seeded with
  `replicaIndex`. `r.jitterRand == nil` disables it (the
  `backoffStep` path skips the shift). Unit tests that assert exact
  Advance(delay) timings must clear `r.jitterRand` after `NewRunner`;
  the existing runner-test fixture does this.

- Listener replicas without app-level `SO_REUSEPORT` opt-in will
  EADDRINUSE on every replica except the first to win the bind race;
  the child crash-loops past `MaxConsecutiveCrashes` and goes FATAL.
  `zpinit --doctor` catches the common Node-version cause pre-boot;
  the config layer can't see what options the interpreter supports.
