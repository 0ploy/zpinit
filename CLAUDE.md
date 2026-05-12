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
images, replacing supervisord + tini + per-image bespoke `docker-entrypoint.sh`
(and PM2 in the Node clustering case). Linux-only in production (Pdeathsig,
/proc); macOS dev compiles via build tags but doesn't exercise PID-1 paths.

Per-phase implementation rationale lives in commit history; `git log` is
authoritative.

## Docs landscape (read before touching the matching code)

User-facing docs are split by concern. When working on a topic, read the
matching doc first to pick up the contract; when changing the topic, update
the doc in the same commit (see "Docs sync" under Conventions).

- `README.md` — three-modes overview, quickstart, replica/cluster pitch.
  Touch when mode behavior, the quickstart, or the operator-command summary
  changes.
- `docs/why.md` — motivation, design decisions, non-goals. Touch when a
  load-bearing design rule or non-goal flips.
- `docs/configuration.md` — full TOML schema and `--check-config` rules.
  Touch on every TOML field add/remove/rename, default change, or
  validation tweak.
- `docs/clustering.md` — replicas, reusePort, PM2 comparison, migration.
  Touch on every replicas/log-path/clustering behavior change.
- `docs/architecture.md` — packages, state machine, boot/reload/shutdown,
  reaping, control protocol. Touch on changes to those internals that
  reshape the contract (not pure refactors).
- `docs/security.md` — threat model, control socket, log handling, env
  injection. Touch on any change to access gates, log open flags, wire
  sanitization, or `[env]` propagation.
- `docs/development.md` — build/test/lint commands and platform notes.

## Load-Bearing Design Rules

**CMD wins over services.** When a CMD is supplied (i.e. `flag.Args()`
non-empty after zpinit parses its own flags), zpinit `syscall.Exec`s it as
PID 1 and ignores `services/` entirely. No CMD → supervise mode, even
when `services/` is empty: the orchestrator boots zero runners, the
control socket comes up, and an operator can add services live via
`zpctl reread` or SIGHUP. This is what makes the published image a
playground (`docker run -it ghcr.io/0ploy/zpinit`) and is why
`detectMode` has only two outcomes. Same image gets three modes from
this rule: production (no CMD → supervise), debug
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

**Control socket has two layered access gates.** (1) Filesystem: bind
runs under `syscall.Umask(0o077)` so the socket is born `0700`, then
explicit `chmod 0600`. Don't drop the umask flip: without it the
socket is briefly `0755` and a non-root local process wins a TOCTOU
race. (2) Peer credentials: every accepted connection is gated by
`SO_PEERCRED` (`authorizePeer` in `peercred_linux.go`) and rejected
unless the peer UID equals the daemon's effective UID. Linux-only;
the macOS dev stub returns `nil`. If you ever loosen socket perms
to allow non-root operators, lift the peer-cred check, don't widen
the chmod.

## Non-Goals (Don't Add These)
No log rotation (use logrotate or stdout → host logging), no log capture
via pipes (FD inheritance only; pipes deadlock), no service dependency
graphs (filename order + readiness probes only), no env interpolation in
configs, no web UI / XML-RPC / metrics endpoint, no Windows / FreeBSD
support, no interactive `zpctl fg`. If a feature request matches one of
these, push back and reconfirm before coding.

## Conventions
- **Docs sync.** Every user-visible behavior change updates the matching
  doc(s) in the same commit, per the "Docs landscape" map above. Also
  append a CHANGELOG.md entry under `## Unreleased` if the change is the
  kind a release note would mention (features, fixes, security). Stale
  docs are worse than missing ones; the release workflow ships whatever
  is on disk.
- No em-dashes (—) in any writing. Use periods, colons, or semicolons.
- Tests use the standard library only. No testify, gomock, or similar.
- Only approved external dependency: `github.com/BurntSushi/toml`. Anything
  else needs explicit approval before `go get`.
- `MaxConsecutiveCrashes = 5` is hardcoded as the retry budget. Spec says
  "retry budget" without naming a config key; promote to config only if asked.
- If a design decision is ambiguous, ask before coding. The architecture is
  the result of multi-round discussion; re-litigating without surfacing
  burns time and risks shipping a different product than intended.
- Don't commit or push on your own. When a change builds, tests green, and
  looks ready, summarize what's staged and ask whether the user wants to
  test it further first or whether it can be committed. They often try
  things in their own environment (docker, real workloads) before a
  commit lands. Pushing without that check skips that step. Same rule
  for tags: never tag or push a tag without an explicit go-ahead.
- GitHub Actions in both workflows are pinned to commit SHAs, never
  floating tags, so a compromised upstream can't inject code into the
  release pipeline (which has GHCR push rights). When upgrading an
  action, dereference the new tag with
  `gh api repos/<owner>/<repo>/git/refs/tags/<tag> --jq '.object.sha'`
  and put the human-readable version in the trailing comment.

## Changelog (CHANGELOG.md)

The release workflow extracts the latest `## vX.Y.Z` section verbatim
into the GitHub release body. Treat CHANGELOG.md as user-facing release
notes, not a commit log.

**When to update.** Every commit that ships user-visible behavior
appends to the *unreleased* top section under the appropriate H3
(`### Features`, `### Bug Fixes`, `### Security`, `### Tests` for
test-only changes worth advertising). Internal refactors, doc fixes,
and CI tweaks usually don't appear; they live in `git log`. If
unsure, ask whether the user would care.

**How to write entries.** Lead with **bold headline**, then a short
prose paragraph that includes *what changed* and *why it matters to
the user* (or *what was broken*, for fixes). Match the voice in
existing entries: no marketing language, no "this commit", no
mention of phase numbers or internal package paths unless they're
load-bearing for users. Reference external behavior, not
implementation. Example shape:

```markdown
- **`zpctl reread` no longer hangs on huge configs.** The diff
  walker held `o.mu` across file reads; with 200+ services on a
  cold cache, control-socket calls timed out. The walk now
  snapshots paths first, then reads outside the lock.
```

**Version heading rules.** New `## vX.Y.Z` sections go at the very
top. The release workflow's awk script extracts everything between
the first `## ` (skipped) and the second `## `, so anything above
the top heading leaks into the release body.

**Cutting a release** (procedure when the user asks for one):

1. Confirm a clean working tree and that CI is green on `main`.
2. In CHANGELOG.md, rename the unreleased section to `## vX.Y.Z`.
   Choose the bump (patch/minor/major) based on the entries: any
   breaking change → major; any new feature → minor; otherwise
   patch. Pre-1.0 we may still bump minor for breaking changes;
   confirm with the user if ambiguous.
3. Commit with message `release: vX.Y.Z` (or fold into the final
   feature commit if the user prefers).
4. Tag: `git tag vX.Y.Z && git push origin main vX.Y.Z`. Don't push
   the tag without confirming first; the tag push triggers a
   public release.
5. After the tag pushes, the `Release` workflow builds binaries,
   pushes the multiarch image to `ghcr.io/0ploy/zpinit`, and
   creates the GitHub release with the latest CHANGELOG section.
   Watch for failures: a failed release leaves the tag in place
   but the artifacts missing.

Don't tag without going through CHANGELOG.md first. The release
body comes from there; an empty or stale top section produces a
release page with nothing useful in it.

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
  `runReloadBoots` takes `o.reloadBootMu` for the duration of its loop
  so back-to-back reloads serialize their boot phases; without that,
  reload N+1's adds could interleave with reload N's still-running
  boots and break filename order. `reloadBootMu` is separate from
  `reloadMu` so the diff phase of N+1 isn't blocked by the boot phase
  of N (which can run for many seconds).

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

- `stopAll` schedules teardown per filename group, in reverse filename
  order BETWEEN groups (filename encodes dependency order so dependents
  drain through their dependencies before the dependency gets SIGTERM)
  and in parallel WITHIN a group (all replicas of one filename are the
  same logical service with no inter-replica flush ordering, so serial
  per-replica teardown would multiply shutdown time by N). The shared
  helper `stopRunnerGroup` is reused by `removeServiceGroup` so the
  reload path gets the same parallel-within-group benefit. Per-runner
  SIGKILL escalation bounds any stuck replica.

- Shutdown wait budget is recomputed at signal time via
  `Orchestrator.ShutdownBudget()`, not snapshotted at boot. It counts
  one `(stop_timeout + reapGrace)` per filename group rather than per
  runner, matching stopAll's parallel-within-group schedule. With
  `replicas = N` a service contributes one unit, not N. Reload can
  add services or bump `stop_timeout` after launch; the supervisor
  outer wait must always cover stopAll's inner wait, otherwise the
  runtime hard-kills PID 1 mid-graceful-shutdown.

- Service log files (`log.stdout`/`log.stderr`) and `cmdTail`'s reads
  open with `O_NOFOLLOW`; a symlink at the leaf of the configured path
  is rejected. `readLastBytes` additionally enforces `Mode().IsRegular()`.
  Standard log-writer hardening: an operator typo or hostile config
  can't cause zpinit to append a child's stdout into `/etc/shadow` via
  a planted symlink. Symlinked parent directories still resolve; only
  the leaf is gated. The parent directory is auto-created with
  `MkdirAll` (mode 0755) just before the open, so operators don't need
  a per-image `entrypoint.d/00-mklogdir.sh`. Only paths the operator
  explicitly named in `[log]` are ever mkdir'd; the `O_NOFOLLOW` leaf
  check is unaffected.

- Wire-protocol responses (`ctlproto.WriteResponse`) sanitize `Msg`
  and every body line via `sanitizeLine`: CR/LF become spaces and a
  lone `.` is prefixed with a space. `cmdTail` surfaces service-
  controlled log content and `cmdUpdate` surfaces multi-line TOML
  errors; without sanitization either could split a single field
  across frames or end the body early at the client.

- `replicas = N` expands one service spec into N first-class
  Runners with the same `cfg.Filename` but distinct `replicaIndex`
  values. The diff key stays at the filename level: `existing` in
  `computeDiffLocked` is `map[string][]*Runner`. Service-spec
  comparison uses `r.Spec()` (the unmodified spec), not `r.Cfg()`,
  because `Cfg().Log.Stdout` may have been per-replica rewritten
  when the path contains `{index}`; comparing `Cfg()` would otherwise
  show a phantom diff every reload. Per-replica log-path rewriting
  and `ZPINIT_REPLICA_INDEX` env injection live in
  `expandServiceToRunners` (replica.go); call it instead of
  `NewRunner` directly when expanding from a spec.

- Replica log paths default to a *shared file* across all N
  replicas. `replicaLogPath` only rewrites when the spec contains
  `{index}`. Linux `O_APPEND` is atomic for writes below `PIPE_BUF`
  (typically 4096 bytes), so concurrent appends from N replicas
  don't tear line-sized log output. Operators wanting per-replica
  files opt in via `{index}` in the path.

- `findRunner(name)` matches by `cfg.Name` only, so for replicated
  services it returns the first replica. zpctl-side verbs that
  target a runner use `resolveTarget(snap, arg)` which parses
  `svc/N` and returns all replicas for the bare-name form. Internal
  callers that don't need replica granularity (the `exit_code_from`
  watcher) keep using `findRunner`; `exit_code_from` is rejected at
  config-load if it points at a replicated service so the ambiguity
  never reaches runtime.

- Listener replicas without app-level `SO_REUSEPORT` opt-in will
  EADDRINUSE on every replica except the first to win the bind race;
  the child crash-loops past `MaxConsecutiveCrashes` and goes FATAL.
  `zpinit --doctor` is the only pre-flight catch for this; the
  config layer can't see whether the running interpreter supports
  the option.
