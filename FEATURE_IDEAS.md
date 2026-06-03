# Feature Ideas

Brainstorm of features that would expand zpinit's value across all three
modes. Items 1-10 started life as "what could mode 1 (Single Process Mode)
offer beyond fleet uniformity and config validation"; many turn out to apply
in modes 2 and 3 too. Items 11+ are a deeper pass on mode 2 (Setup, then
Run Mode) specifically.

Status of every entry: under discussion. None are committed. The purpose of
this file is to have a written record so we can come back, sharpen, accept,
or reject each idea with rationale.

## 1. External preconditions: `[[wait]]`

Container-level pre-flight gates. Block boot until external dependencies are
reachable.

```toml
# zpinit.toml
[[wait]]
type    = "tcp"
target  = "db:5432"
timeout = "30s"

[[wait]]
type = "http"
url  = "http://configd/healthz"
```

Applies in all three modes, before exec / entrypoint.d / services. Replaces
the wait-for-it.sh / depends_on workaround.

Distinct from mode 3's `[ready]`:

- `[ready]` is self-readiness, per service, gates the next service in
  filename order. The probe targets a process zpinit just spawned.
- `[[wait]]` is external-dependency-readiness, container-scope, gates the
  whole boot. The probe targets a service in another container that this
  image has no control over.

Implementation note: factor probe execution into one internal package shared
with `[ready]` so probe semantics (timeout, error format, retry) stay
identical across the codebase.

Open questions:

- Probe types in v1: `tcp`, `http`, `command`?
- Top-level `wait_timeout` default, or per-entry only?
- On timeout: hard fail, or proceed with a warning?

## 2. Required preconditions: `[require]`

Static assertions evaluated before exec / entrypoint.d / services. Fast-fail
with a clear message instead of crashing 200ms in.

```toml
[require]
env    = ["DB_HOST", "DB_PASSWORD"]
mounts = ["/var/lib/myapp", "/etc/secrets/"]
```

Especially valuable in mode 1 where there is no entrypoint.d to host these
checks. Doubles as a vehicle for org-wide policy (e.g. "`IMAGE_VERSION` must
be set on every container").

Open questions:

- `mounts` semantics: dir-must-exist, dir-must-be-non-empty, or
  dir-must-be-an-actual-mountpoint?
- Distinguish "env var defined" from "env var defined and non-empty"?

## 3. Runtime normalization: `[runtime]`

Container-level housekeeping that today is scattered across base images and
frequently inconsistent.

```toml
[runtime]
ulimit_nofile = 65535
umask         = "0022"
tz_default    = "UTC"
ensure_dirs   = ["/tmp", "/var/run/myapp"]
```

Applied in mode 1 before exec, in modes 2/3 before scripts/services start.
Single fleet-wide policy lever.

Open questions:

- Other ulimits worth exposing (`nproc`, `memlock`)?
- `ensure_dirs` mode/owner: hard-coded `0755 root:root`, or per-entry?
- Conflict policy when `ensure_dirs` overlaps with mounted volumes?

## 5. Swiss-army subcommands

Even when zpinit `exec`s away in mode 1, the static binary sits in every
image. Expose generic utilities as subcommands and delete a class of
dependencies (`nc`, `wait-for-it.sh`, `gettext`/envsubst, parts of
`coreutils`):

```
zpinit wait-tcp host:port --timeout 30s
zpinit wait-http URL
zpinit envsubst < tpl > out          # file rendering only, NOT in-config
zpinit chown-tree app:app /var/lib/myapp
zpinit ensure-dirs /tmp /var/run/myapp
```

Likely the highest leverage single addition: operators get one vocabulary at
build time and at runtime, regardless of mode. Reuses the probe package
from (1).

Open questions:

- Scope creep: where do we draw the line? "Things zpinit already needs
  internally" is a defensible boundary.
- `envsubst` substitution syntax: shell-style `${VAR}`, Go template, or both?

## 6. Drop privileges: `[run] user/group`

Replace `gosu` / `su-exec` for the common mode-1 case. zpinit setuid/setgid
in pre-exec, then `syscall.Exec`. One less binary in slim images.

```toml
[run]
user  = "app"
group = "app"
```

Open questions:

- Interaction with mode 3 services that already accept per-service
  `user`/`group`. Does `[run]` set a default they can override?
- Capability handling: drop all caps by default, or leave unchanged?

## 7. File ownership normalization: `[[chown]]`

Replace the most common entrypoint.sh chore in php / ruby / rails images.

```toml
[[chown]]
path      = "/var/lib/myapp"
user      = "app"
group     = "app"
recursive = true
```

Idempotent, no shell quoting, runs in mode 1 too.

Open questions:

- Performance on `recursive = true` for huge volumes; do we cap or just
  document?
- Overlap with `[runtime].ensure_dirs`: merge, or keep distinct (ensure-dirs
  creates, chown adjusts existing)?

## 8. Supervise-one-process shorthand: `--supervise` (or `--reap-only`)

```dockerfile
ENTRYPOINT ["zpinit", "--supervise"]
CMD ["my-app"]
```

zpinit fork+waits, reaps, forwards signals, prints structured exit log.
Equivalent to mode 3 with one service file but zero TOML.

Tradeoffs:

- Adds a fourth conceptual mode (or stretches mode 1's definition).
- CLAUDE.md has a load-bearing rule against "supervise + main task"
  combined modes. This is not quite that (no services involved), but it
  does brush against it.
- Narrower `--reap-only` framing might be more honest about scope.

Open questions:

- Is the mode-3-with-one-service path good enough, or does the activation
  cost (write a TOML file) actually block adoption?
- If we add this, how do we explain it without confusing newcomers about
  mode boundaries?

## 9. First-run / one-shot hooks: `[[once]]`

Native version of the "run migrations once" pattern.

```toml
[[once]]
marker  = "/var/lib/myapp/.initialized"
command = ["my-app", "db", "migrate"]
```

Open questions:

- Substantial overlap with mode 2 entrypoint.d (you can write the same
  pattern as a 5-line script). Is the declarative form enough of a win to
  carry a new top-level concept?
- Marker race: what happens if two zpinit processes start concurrently?
  (Shouldn't happen in our world, but worth defining.)

## 10. Secrets management: fetch and inject

Pull secrets from a secrets store at boot and make them available to the
workload. Replaces the per-image pattern of "shell out to `aws ssm
get-parameter` from entrypoint.sh", a Vault Agent sidecar, or shipping
plaintext via env vars on `docker run -e`.

```toml
[[secret]]
name    = "DB_PASSWORD"     # env var injected into workload
source  = "vault"           # backend identifier
path    = "secret/data/myapp/db"
key     = "password"

[[secret]]
name    = "tls.key"
source  = "aws-sm"
path    = "myapp/tls"
key     = "key"
target  = "file:/run/secrets/tls.key"   # tmpfs file instead of env
mode    = "0400"
owner   = "app"
```

Mode interactions:

- Mode 1: fetch before exec, inject as env into the exec'd CMD.
- Mode 2: fetch before entrypoint.d, scripts inherit them.
- Mode 3: fetch before service boot, inject into baseEnv / per-service env.

Fleet leverage: one place to update backend auth, one place to add a new
backend, one place to enforce "never log values." For a 130-image fleet
this is a high-value central choke point. Avoids the proliferation of
"my image's entrypoint.sh shells out to its own SDK" pattern.

Avoiding the "no env interpolation in configs" non-goal: secrets are
declared via structured `[[secret]]` blocks, NOT via `${VAULT:...}`-style
substitution inside other config values. Other zpinit fields don't
interpolate from secrets; secrets only flow into the workload's env or
files.

### Injection patterns

Two patterns, picked per-secret based on what the consumer supports:

**Pattern A: env var holds the value.** Consumer reads `DB_PASSWORD`
directly. Simple, broadly compatible, but the value is visible in
`/proc/<pid>/environ` to anything that can read that node (typically
same-UID processes).

**Pattern B: tmpfs file, env var holds the path.** The "Docker
secrets" / "Kubernetes secret volume" model. Many shop runtimes already
support a `_FILE` suffix convention (PostgreSQL, MySQL, Redis,
phpMyAdmin, Composer, plenty of others) where setting
`DB_PASSWORD_FILE=/run/secrets/db_password` makes the consumer read
the file and treat its contents as the value.

```toml
[[secret]]
source     = "vault"
path       = "secret/data/myapp/db"
key        = "password"
target     = "file:/run/secrets/db_password"  # value written here
file_mode  = "0400"
file_owner = "app"
env        = "DATABASE_PASSWORD_FILE"          # env var pointing at the path
```

Implementation sketch:

```go
path := fmt.Sprintf("/run/secrets/%s/db_password", serviceName)
os.MkdirAll(filepath.Dir(path), 0700)
os.WriteFile(path, []byte(value), 0400)
os.Chown(path, childUID, childGID)
cmd.Env = append(cmd.Env, "DATABASE_PASSWORD_FILE="+path)
```

For shop runtimes this is often the cleanest pragmatic answer: the
secret never lands on disk that survives container exit (tmpfs only),
permissions are tight enough that other UIDs in the same container
can't read it, and the env var only carries a path so it's harmless to
log.

The mount under `/run/secrets/` MUST be tmpfs. If it falls back to a
disk-backed overlay (because the operator didn't declare the mount,
or the runtime ran out of memory and swapped), the secret persists
across restarts as a file on disk. zpinit should refuse to write a
secret file if the target path's filesystem isn't tmpfs.

### Per-child isolation (mode 3)

Mode 3 spawns multiple services. If they all run as the same UID with
shared FDs, they can read each other's secrets via `/proc/<pid>/environ`,
ptrace, or inherited file descriptors. The discipline that prevents this:

- **Different UID per service.** The single biggest isolation win.
  Without it, `/proc` cross-readability and `ptrace` put every secret
  one well-aimed `cat` away. Mode 3 services already accept
  `user`/`group` in their TOML; the secrets writer has to chown each
  per-service tmpfs path to match the consuming service's UID/GID.
- **Per-service tmpfs subdirectory.** `/run/secrets/<service>/...` with
  the directory itself `0700` and owned by the service's UID. Sibling
  services can't even `ls` it.
- **Don't leak FDs across spawns.** Go's `os.Pipe()` sets `O_CLOEXEC`,
  but `cmd.ExtraFiles` deliberately clears it for the FDs you pass in.
  Any pipe used to deliver secrets to service A's stdin must be
  `Close`d in the supervisor immediately after `cmd.Start()`, otherwise
  service B inherits a copy of the read end during its later spawn.
- **Don't blindly inherit supervisor stderr/stdout into children.** If
  zpinit ever logs anything secret-adjacent (a fetch error containing
  the env name plus a partial value), and a child inherits stderr, the
  child sees it. Mode 3 already routes service stdio per-service; worth
  re-auditing once secrets land.
- **`Pdeathsig: syscall.SIGTERM` on every child.** A supervisor crash
  must take the children with it; otherwise crashed-zpinit leaves
  children alive with old secrets in their env. zpinit already sets
  this for service children today; secrets inherit the protection.
- **Reconcile on supervisor restart.** If zpinit restarts (planned or
  via crash + container-runtime relaunch) and finds `/run/secrets/`
  populated, the safe default is to wipe it and re-fetch. Stale
  secrets persisted across a supervisor restart are an exfiltration
  surface.

### FD-passing alternative

For services we control (custom internal tooling) that can read secrets
from a numbered FD, pass via pipe and don't write to a file at all:

```go
r, w, _ := os.Pipe()
cmd.ExtraFiles = []*os.File{r}        // FD 3 in the child
cmd.Env = []string{"SECRETS_FD=3"}    // minimal env, no value, no path
cmd.SysProcAttr = &syscall.SysProcAttr{
    Setsid:     true,
    Pdeathsig:  syscall.SIGTERM,
    Credential: &syscall.Credential{Uid: childUID, Gid: childGID},
}
cmd.Start()
r.Close()                             // supervisor drops its copy NOW
go func() {
    defer w.Close()
    json.NewEncoder(w).Encode(mySecrets)
}()
```

Strictest isolation: nothing on disk, nothing in env, no `/proc`-visible
target. But it requires the consumer to participate, so it's the niche
path. Most shop services in our fleet will use the tmpfs+`_FILE`
pattern (B) above. Worth supporting eventually but not in v1.

### Hard requirements regardless of design

- Never log fetched values (treat the redaction story as a feature, not
  an afterthought; `--check-config` and `zpctl status` must redact).
- Files written under `target = "file:..."` must be created in tmpfs and
  cleaned up at shutdown.
- Boot must hard-fail if a required secret can't be resolved (don't
  silently boot a half-configured app).

### Scope-bounding open questions (potentially big)

- Which backends in v1? Defensible minimum: `file` (read from a path,
  trivial) and `command` (exec a user-supplied helper that prints the
  value, delegates auth entirely). Vault / AWS-SM / GCP-SM / Azure-KV
  added later as built-ins, OR left to the `command` backend forever.
- Auth: if we ship native backends, how does zpinit auth to them?
  (Vault: app role, K8s SA token, env-provided token. AWS: IAM role,
  env, config file.) Each adds dependency surface.
- Rotation / refresh: out of scope for v1 (one-shot at boot only)?
  Rotation forces zpinit to stay alive and re-spawn or signal services,
  which is a much bigger feature.
- Memory hygiene: zero buffers after use? Go makes this best-effort at
  best; document the limitation.
- Ordering vs. `[require]`: secrets must resolve before `[require]` env
  checks run, otherwise required envs sourced from secrets always fail.
- Precedence vs. `[env]`, container env, `scriptEnv`: where do secrets
  sit in the chain? Probably "above container env" so they can't be
  accidentally clobbered by `docker run -e`.
- Plugin architecture or hard-coded backends? Plugin (separate binary
  invoked over stdin/stdout) keeps the static-binary story clean and
  caps blast radius; hard-coded is simpler but balloons dependencies.

This is a big feature. I'd lean toward shipping `file` + `command` first
(minimal surface, proves the injection plumbing) and only adding native
backends if the `command` pattern proves insufficient.

## Mode 2 (Setup, then Run) focus

Mode 2's contract today is simple: run executables in `entrypoint.d/` in
filename order, drain zombies, exec the CMD. That contract is healthy and
worth preserving. The pain points operators hit are around the
*individual script* surface and the *cross-cutting boot story*:

- Per-script config (timeout, user, cwd, retries, idempotency) requires
  shell-level if/case nests in every script.
- Script output is interleaved on stdout in `docker logs`, hard to triage
  when a script three steps deep fails.
- `/run/zpinit/env` is the only way to pass data forward, with no
  type-checking, no quoting story, no atomic write, no read-back API.
- Adding setup steps to images we don't fully control means rebuilding
  them; there's no operator-mounted override path.
- Once items 1-10 land, there are many phases (waits, requires, runtime,
  chown, secrets, render, entrypoint.d). The canonical order needs to be
  defined and documented, not implicit in code.

The features below address these.

## 11. Declarative entrypoint hooks: `[[entrypoint]]`

Express setup work in `zpinit.toml` directly, alongside (not replacing)
`entrypoint.d/`:

```toml
[[entrypoint]]
name    = "wait-db"
command = ["zpinit", "wait-tcp", "db:5432"]
timeout = "30s"

[[entrypoint]]
name    = "migrate"
command = ["my-app", "db", "migrate"]
once    = "/var/lib/myapp/.migrated"
user    = "app"
retries = 3
backoff = "5s"
when    = "env:RUN_MIGRATIONS=true"
```

Why: an org-wide policy hook ("every container must call out to a fleet
registry") becomes a base-image `zpinit.toml` change rather than dropping
a shell script into 130 `entrypoint.d/` directories. Per-hook config
(timeout, user, cwd, env, once-marker, retries, when-condition) replaces
shell boilerplate. Subsumes ideas (8) `[[once]]` and various
mode-1 retry/condition flags into a single coherent unit.

Open questions:

- Run order vs. `entrypoint.d/`: hooks first (org policy before image
  customization), or hooks last, or filename-merged with `entrypoint.d`?
- Do `[[entrypoint]]` blocks honor the global `entrypoint_on_failure`,
  or override it with a per-block `on_failure`?
- `when` syntax: shell-like (`env:X=true`), CEL-like, or limited to a
  small DSL (`env:X`, `env:X=y`, `file:/path/exists`)?

## 12. Per-script config sidecars: `<script>.toml`

For users who keep `entrypoint.d/` scripts, allow optional sidecar TOML
to attach config without inventing front-matter or rewriting the script
as an `[[entrypoint]]` block:

```
entrypoint.d/
  10-migrate.sh
  10-migrate.toml      # optional: timeout, user, cwd, once, retries, when
```

Sidecar fields mirror `[[entrypoint]]` (item 11). Resolution: filename
match minus `.toml`. Missing sidecar means defaults.

Open questions:

- `--check-config` should warn on orphan sidecars (no matching script)?
- Conflict policy if both an `[[entrypoint]]` block and a sidecar
  reference the same step name? Probably refuse with a validation error.

## 13. Config templating: `[[render]]`

The most common entrypoint.sh chore after chown. `envsubst < tpl > out`,
declaratively:

```toml
[[render]]
template = "/etc/nginx/nginx.conf.tpl"
output   = "/etc/nginx/nginx.conf"
mode     = "0644"
owner    = "app"
```

Reuses the `zpinit envsubst` subcommand internally so behavior is
identical between "called via `[[render]]`" and "called manually from a
script." Templating runs on EXTERNAL files only, so the
"no env interpolation in configs" non-goal is preserved (other
`zpinit.toml` fields don't get substituted).

Open questions:

- Substitution syntax: shell-style `${VAR}` to start; add Go template
  via `engine = "go"` later?
- Failure policy when a template references an unset env var: empty
  string, leave literal, or error?
- Atomic write (temp + rename) by default? Likely yes.

## 14. `zpinit env` subcommands

Today, scripts append to `/run/zpinit/env` by hand:

```sh
echo "REGION=eu-west-1" >> /run/zpinit/env
```

Footguns: format errors silently break propagation, no quoting story,
no atomic write, no read-back, no namespacing. Replace with subcommands
(item 5 already proposed swiss-army subcommands in general; this is the
specific entry):

```
zpinit env set REGION eu-west-1
zpinit env get REGION
zpinit env list
zpinit env unset REGION
```

Internally writes `/run/zpinit/env` with proper quoting, atomic
temp+rename, and meaningful exit codes. Same propagation guarantees as
today, cleaner interface.

Open questions:

- Multi-line / binary values: support, escape, or refuse with a clear
  error?
- Type-checking at write time (`--type=int`) or just store-and-forward?

## 15. Multiple entrypoint directories

```toml
entrypoint_dirs = [
  "/etc/zpinit/entrypoint.d",      # baked into image
  "/run/zpinit/entrypoint.d",      # operator-mounted at runtime
]
```

Files merged alphabetically across all listed dirs. Lets fleet operators
inject org-wide setup into images they don't control by mounting a
volume; also a clean way to layer `docker run -v` debug overrides without
rebuilding.

Open questions:

- Filename collision policy: error, last-wins, or skip-with-warning?
- Default value: just `/etc/zpinit/entrypoint.d` for back-compat, or
  also `/run/zpinit/entrypoint.d` automatically as a fleet convention?

## 16. Per-script and per-phase timing envelope

Wrap each script with structured zpinit-stdout lines. Does NOT capture
the script's own stdout/stderr (which stay inherited), so no conflict
with the no-pipes non-goal:

```
[zpinit] entrypoint  /etc/zpinit/entrypoint.d/10-migrate.sh start
... script's inherited stdout/stderr appears here ...
[zpinit] entrypoint  /etc/zpinit/entrypoint.d/10-migrate.sh exit code=0 dur=4.2s
```

Plus a one-shot phase summary on success:

```
[zpinit] phase summary
  wait        : 3.2s (db:5432)
  require     : 0.0s
  runtime     : 0.1s
  chown       : 0.4s
  secrets     : 0.8s (3 fetched)
  render      : 0.1s (3 templates)
  entrypoint  : 6.8s (5 scripts)
  total       : 11.4s
```

Cheap, fleet-uniform `docker logs` parsing surface, materially helpful
for tuning slow boots. Builds on the boot banner from item 4 with the
same opt-out flag.

Open questions:

- Format: key=value or single-line JSON per line?

## 17. Per-script stdout/stderr to files (FD inheritance)

Mirror mode 3's `log.stdout` / `log.stderr` semantics for entrypoint.d:

```toml
[entrypoint_logs]
dir = "/var/log/zpinit/entrypoint"
```

Per-script files appear at `<script-name>.stdout` /
`<script-name>.stderr`. Implementation reuses the existing FD-inheritance
log writer with `O_NOFOLLOW` and regular-file gating, so it's compatible
with the no-pipes rule.

Why: today, when `30-migrate.sh` fails at minute 4, its output is
interleaved with every other script's output in `docker logs`. Per-script
files mean `tail` works.

Open questions:

- Default on or off? On adds disk usage; off keeps current behavior.
- Rotation: explicitly no (matches the "no log rotation" non-goal),
  document it.

## 19. Parallel execution within priority bands (opt-in)

Today `entrypoint.d/` is strictly sequential. Scripts that share a
numeric prefix could run in parallel:

```
entrypoint.d/
  10-fetch-region.sh    (independent)
  10-fetch-cluster.sh   (independent)
  20-render-config.sh   (depends on 10s)
```

Opt-in via `parallel_within_priority = true` in `zpinit.toml`. `20-*`
waits for ALL `10-*` to finish.

Honest assessment: this is the weakest item in the mode-2 list. It
breaks the "filename order is gospel" mental model that's currently
load-bearing for `[ready]` ordering in mode 3. Two `10-*` scripts both
writing `/run/zpinit/env` is racy. Empirically the speedup may be
small. Including for completeness; would defer indefinitely unless real
slow-boot data shows up.

## Phase ordering (canonical sequence)

Once any of items 1-10 and 11-19 ship, the boot pipeline has many
phases. The canonical order matters and should be documented from the
first feature onward, not retrofitted. Proposal:

1. Parse and validate config.
2. Boot banner (item 4).
3. `[[wait]]` external preconditions (item 1).
4. `[require]` assertions (item 2).
5. `[runtime]` normalization (item 3).
6. `[[chown]]` ownership (item 7).
7. `[[secret]]` resolution (item 10).
8. `[[render]]` templating (item 13).
9. `[[entrypoint]]` zpinit.toml hooks (item 11).
10. `entrypoint.d/` scripts (existing mode 2).
11. Mode dispatch:
    - mode 1: `syscall.Exec(CMD)`
    - mode 2: drain zombies, then `syscall.Exec(CMD)`
    - mode 3: start services in filename order, stay alive as PID 1.

Rationale embedded in the order:

- Secrets resolve BEFORE templates so templates can reference
  secret-sourced env (`${DB_PASSWORD}`).
- Assertions run AFTER waits so "DB_HOST must be set AND db must be
  reachable" is a coherent two-phase check.
- Chown / render run BEFORE `entrypoint.d/` so scripts see the final
  filesystem layout.
- `[[entrypoint]]` (org policy) runs BEFORE `entrypoint.d/` (image
  customization) so policy-set env / files are visible to scripts.

This ordering is itself a design decision. Worth committing to docs the
first time we ship any of these phases.

## Mode 3 (Manage Services) focus

Mode 3 is the most feature-rich mode today: filename-ordered boot, `[ready]`
probes, FD-inheritance log files, backoff, reload via SIGHUP, control
socket gated by `SO_PEERCRED`, reverse-serial shutdown with SIGKILL
escalation, `exit_code_from` for the foreground-worker pattern. The
contract is solid; the gaps are around runtime observability,
graceful-drain semantics, per-service isolation, and operator tooling
ergonomics.

Hard constraints from `CLAUDE.md` to keep in mind throughout:

- No service dependency graphs (filename order + `[ready]` only).
- No interactive `zpctl fg`.
- No log piping / capture.
- No log rotation.
- No web UI / metrics endpoint.

The features below sit inside those guardrails.

### Pain points

- `[ready]` is boot-time only. Once a service passes its readiness probe,
  zpinit doesn't track its health. A deadlocked-but-not-crashed process is
  invisible.
- Crash visibility is thin. Logs go to inherited FDs (good), but there's
  no structured "service X crashed at T with code C, here's the recent
  context" envelope, and no historical record across restarts.
- No native graceful-drain. `pre_stop` work happens inside the app's
  SIGTERM handler, which mixes ops concerns into application code.
- Single SIGTERM-then-SIGKILL stop ladder. nginx wants `QUIT` for
  graceful, php-fpm wants `QUIT` then `TERM`, custom apps want
  intermediate signals. Today every team hand-rolls a wrapper.
- No per-service resource limits. One runaway worker can OOM the whole
  container; the kernel picks who dies, not zpinit.
- `MaxConsecutiveCrashes = 5` is hardcoded. Spec calls it a "retry
  budget" without naming a config key (`CLAUDE.md` notes this; promote
  if asked).
- Reload restarts on any TOML change, including comment-only or
  log-path-only edits, which is expensive for stable services.
- No container-level readiness query. Schedulers wanting "is the whole
  pod ready for traffic" have to grep `zpctl status` output.

## 20. Liveness probes: `[live]`

Continuous health checks for already-running services, with restart on
sustained failure. Distinct from `[ready]`, which fires once at boot to
gate the next service.

```toml
command = ["redis-server", "--daemonize", "no"]
restart = "always"

[ready]
command  = ["redis-cli", "ping"]
interval = "500ms"
timeout  = "30s"

[live]
command  = ["redis-cli", "ping"]
interval = "30s"
timeout  = "5s"
failures = 3      # restart after N consecutive probe failures
```

Why: a deadlocked but not-crashed service is invisible to PID-1 reaping;
liveness probes catch it. K8s does this; supervisord doesn't. Real fleet
value for shop runtimes that occasionally wedge.

Open questions:

- Probe-the-probe failure: if the probe binary itself errors (not the
  service), is that a service failure or a probe failure? Probe failure
  should not count toward `failures`.
- During graceful stop, suppress liveness probes (otherwise stop-window
  failures cause a phantom restart attempt).
- Reuse the shared probe package from item 1 / `[ready]`.

## 21. Per-service resource limits: `[limits]`

Today services inherit the container's rlimits. Per-service caps would
let zpinit enforce "the worker doesn't get to OOM the rest of the
container":

```toml
[limits]
memory_max    = "256MB"   # RLIMIT_AS
cpu_time_max  = "30s"     # RLIMIT_CPU (per process; not wall clock)
nofile        = 4096
nproc         = 100
oom_score_adj = 500       # OOM-kill this service before its siblings
nice          = 10
```

Implementation: `setrlimit` in the pre-exec hook for the spawned child;
write to `/proc/<pid>/oom_score_adj` after spawn (or as part of clone
flags). All Linux-only and fits the existing `peercred_linux.go` /
`reaper_linux.go` shape.

Open questions:

- cgroup vs. rlimit for memory: rlimits don't catch all kernel-side
  allocations and aren't enforceable on all paths. cgroup is more
  accurate but requires writable `/sys/fs/cgroup` and varies between
  v1/v2. v1: rlimit only; cgroup later if needed.
- Interaction with container-wide `[runtime].ulimit_nofile` (item 3): per-
  service `[limits].nofile` overrides; otherwise container default
  applies.

## 22. Pre-stop hook: graceful drain before SIGTERM

```toml
[hooks]
pre_stop         = ["my-app", "drain-connections"]
pre_stop_timeout = "10s"
```

Runs on shutdown (and on `zpctl stop`/`restart`) before the configured
stop signal, with its own timeout. If the hook exits non-zero or times
out, zpinit proceeds with the stop signal anyway and logs the failure.

Why: today, drain logic ("stop accepting new connections, flush, then
exit") lives inside the app's SIGTERM handler, mixing ops concerns
into application code. A native hook lets ops own it. Especially
useful for php-fpm pools, message-queue workers in mid-job, and
nginx-style "wait for in-flight requests."

Open questions:

- Run before stopping siblings or interleaved with the reverse-serial
  stop sequence? Before, otherwise siblings already gone when drain
  needs them (e.g., draining nginx needs php-fpm still alive).
- `post_stop` hook (cleanup after process exits): worth it, or
  scope-creep? Probably defer unless asked.

## 23. Audit trail and crash log

Structured event log of every supervisor decision (start, stop,
restart, signal, reload, crash), accessible via control socket.

```
[zpinit] audit service=redis  event=start    pid=42  reason=initial-boot       ts=...
[zpinit] audit service=redis  event=ready    pid=42  duration=420ms            ts=...
[zpinit] audit service=redis  event=exit     pid=42  code=137 signal=KILL      ts=... lifetime=4h12m
[zpinit] audit service=redis  event=restart  pid=43  reason=auto-restart       ts=... attempt=1/5
```

Two surfaces:

- `zpctl audit [service] [--since=...]`: tail-style output on
  zpinit's stdout. Reuses the timing-envelope format from items 4 / 16.
- `zpctl crashlog [service]`: in-memory ring buffer of recent crashes
  per service (last N, where N is small; ~10 per service). Each entry
  includes timestamp, exit code, signal, lifetime, last K stderr lines
  pulled from the FD-inheritance log file via the existing `cmdTail`
  path.

Why: today, "why did this service restart?" requires correlating
service log timestamps with `zpctl status`. A first-class audit stream
makes post-mortem trivial.

Open questions:

- In-memory only, or persisted? Persisted survives zpinit restart but
  adds a write path. v1: in-memory only; persistence later if asked.
- Audit on stdout vs. on a separate FD: stdout matches the boot-banner
  pattern from item 4 and is easiest for `docker logs`. Risk: noisy.
  Probably needs a `audit_level = warn|info|debug` knob.

## 24. Configurable backoff and retry budget

Promote `MaxConsecutiveCrashes` from hardcoded constant to config
(`CLAUDE.md` says do this only when asked; treating that as "asked"
once we ship `[on_crash]` or per-service overrides).

```toml
# zpinit.toml: container-wide defaults
[backoff]
max_consecutive = 5
initial         = "1s"
multiplier      = 2.0
cap             = "60s"

# per-service override (in services/foo.toml)
[backoff]
max_consecutive = 10
```

Open questions:

- Time-window-based ("max 5 crashes per 5 minutes") in addition to
  consecutive-count? Window-based is more forgiving for services that
  recover; consecutive count is simpler and matches today.
- What happens after `max_consecutive`? Today: service is left dead,
  supervisor continues. Future: pluggable via `[on_crash]` from
  item 29.

## 25. Smart reload diffing

Today, any byte-level change to a service TOML triggers a restart on
reload. A log-path-only edit, a comment-only edit, or a whitespace
change all restart the service. Smart diff: only restart when
execution-relevant fields change (`command`, `args`, `env`, `user`,
`group`, `cwd`, `[limits]`, `[hooks]`).

Non-execution-relevant fields (`log.stdout`, `log.stderr`, `[ready]`
probe config, `[live]` probe config, comments) take effect without
restart.

Open questions:

- `[ready]` is boot-time only, so reloading it for a running service
  is a no-op anyway. `[live]` reloads in place: cancel the current
  probe loop, start a new one with the new config.
- Log-path change in place: reopen the log files (rotation-friendly
  too) without restart. Already partially in place for SIGUSR1?

## 26. Multi-signal stop ladder

Today's stop sequence is essentially one signal then SIGKILL escalation.
Some apps want intermediate signals:

```toml
stop_signals = [
  { signal = "TERM", wait = "10s" },
  { signal = "QUIT", wait = "5s"  },
  { signal = "KILL" },
]
```

Why: nginx graceful (`QUIT`), php-fpm (`QUIT` then `TERM`), apps with
custom mid-shutdown signals all currently need wrapper scripts.

Open questions:

- Backwards compatibility with the existing single `stop_signal`
  field. Probably: if `stop_signals` is set it wins; otherwise fall
  back to single-signal behavior.
- Total budget: sum of `wait` values must not exceed the
  `Orchestrator.ShutdownBudget()` already recomputed at signal time
  (per `CLAUDE.md`). Validate at config load.

## 28. Better `[ready]` failure diagnostics

When boot times out because `[ready]` never passed, today's error is
short. Could be much richer:

```
[zpinit] service redis: [ready] probe never passed within 30s
  probes attempted: 60 (interval=500ms, last attempt 21ms ago)
  last error  : connection refused (target=localhost:6379)
  process state: PID 42 RUNNING (rss 1.2MB, uptime 30s)
  recent stderr (last 5 lines via log.stderr file):
    1:M Oct 13 14:21:02.001 # Could not bind to port 6379
    1:M Oct 13 14:21:02.002 # Address already in use
    ...
```

Why: a real "boot stuck" debugging session today involves manually
pulling logs, comparing to `zpctl status`, etc. Bundling the context
into the failure message saves operator minutes.

Open questions:

- Cost of carrying probe-attempt state per service vs. just printing
  "tried 60 times." Latter is cheaper and probably enough.
- Should we always print this on `[ready]` timeout, or only on a
  verbose flag?

## 29. Crash actions: `[on_crash]`

When a service exhausts its retry budget (item 24), today it's
left dead and the supervisor keeps running. Some operators want
escalation:

```toml
[on_crash]
action  = "restart"          # default; current behavior
# OR
action  = "shutdown"         # whole container exits, scheduler picks up
# OR
action  = "exec"
command = ["/usr/local/bin/notify-ops", "{{service}}", "{{exit_code}}"]
```

Why: lets ops choose the escalation policy explicitly per service.
"PHP-FPM crashed 5 times in a row, take the whole container down so
k8s reschedules" is a legit policy. Today only the `exit_code_from`
foreground-worker pattern offers this, and only for one service.

Open questions:

- `action = "exec"` brings template substitution back (`{{service}}`,
  `{{exit_code}}`). Either pass via env vars (no in-config interpolation,
  matches the non-goal) or define a small template syntax limited to
  this field. Env vars are cleaner.
- Combinable with the foreground-worker `exit_code_from`? Probably
  the foreground-worker rule wins; document the precedence.

## 30. File log forwarding: surface file-only logs as stdout/stderr

Some legacy or third-party software can only write logs to a file, not
stdout/stderr (Java apps with log4j-XML pinned to a path, PHP with
`error_log = /var/log/php/errors.log`, MySQL slow-query log, plenty of
others). Today the workaround is bespoke per image: a
`tail -F file >&2` background script in `entrypoint.d/`, or a symlink
from the log path to `/dev/stdout`. Bake this into the service TOML so
the workaround disappears:

```toml
# services/20_legacy-app.toml
command = ["/usr/local/bin/legacy-app"]
restart = "always"

[[log_forward]]
path   = "/var/log/legacy/app.log"
target = "stdout"

[[log_forward]]
path   = "/var/log/legacy/error.log"
target = "stderr"
prefix = "[error] "      # optional, applied per line
```

`target` accepts `stdout`, `stderr`, or a path (reusing the existing
`[log]` destination machinery with `O_APPEND|O_NOFOLLOW`).

Two implementation strategies, both worth considering:

**Strategy A: tail-and-forward (file-watch).** zpinit opens the file
for read, seeks to end, watches via inotify (with polling fallback) and
forwards new bytes to the target. Works with any writer behaviour:
truncate-on-start, log rotation by the app, exclusive opens, `O_APPEND`
writers. The writer never blocks on a slow zpinit because there's no
pipe between them. The writer writes to the kernel; zpinit reads from
the kernel. So the load-bearing "no pipes" rule is preserved. The file
still lives on disk (or tmpfs) and grows; rotation stays the operator's
job.

**Strategy B: pre-create the file as a symlink to `/dev/stdout` /
`/dev/stderr`.** The classic Docker idiom
(`ln -sf /dev/stdout /var/log/nginx/access.log`) baked into zpinit
instead of into a Dockerfile RUN line. Almost zero runtime cost, no
data path through zpinit, works for any app that respects the symlink.
Fails for some app behaviours:

- Apps that truncate the file on startup break (you can't truncate
  `/dev/stdout`).
- Apps that unlink-and-recreate (logrotate-style) break the symlink.
- Apps that use `O_CREAT|O_EXCL` reject an existing symlink.
- Apps that `lseek` on the fd: character devices don't support seeking.

The two strategies could coexist as `mode = "symlink"` (well-behaved
apps) vs. `mode = "tail"` (everything else). Default `tail` since it
always works; operators opt in to `symlink` when they know the app is
compatible.

Why bake either of these into zpinit:

- Replaces a per-image `entrypoint.d/`-and-shell-script pattern that's
  currently bespoke per shop, frequently unreaped, and silently breaks
  on log rotation.
- For base images we don't fully control (where adding a wrapper script
  means rebuilding upstream), this is the only sane path to "this app's
  logs go to docker logs."
- Lets us drop the per-shop `tail -F` background processes that today
  clutter the supervisor's child-process list.

Open questions:

- Inotify on tmpfs / overlayfs / weird container filesystems is known
  to be flaky across runtimes. Polling fallback with a configurable
  interval covers it.
- Rotation behaviour: follow rename to a new inode (matches `tail -F`)
  or stay on the original fd (`tail -f`)? Default `tail -F` semantics.
- Initial-content policy: replay existing file content on service
  start, or only forward bytes appended after start? Default: only
  new.
- Multi-line records (Java stack traces, multi-line PHP warnings):
  forward line-by-line, or buffer atomic blocks via a configurable
  line-prefix regex? v1: line-by-line; structured forwarding deferred.
- Interaction with `[log] stdout = "..."` (the service's own fd 1/2
  file destination): independent. `[[log_forward]]` watches files the
  app writes itself; `[log]` redirects fd 1/2 at spawn.
- Disk pressure: an unrotated forwarded file grows without bound.
  Document that operators arrange rotation or a tmpfs cap; zpinit
  doesn't truncate (the "no log rotation" non-goal still applies).
- Mode 2 applicability: the same need shows up in `entrypoint.d/`
  scripts that drive a file-logging tool. Could generalise
  `[[log_forward]]` to be container-scope (in `zpinit.toml`) rather
  than service-scope, applied across modes.

## Mid-tier mode-3 ideas (no dedicated section)

- **Service groups** (`group = "web"`) plus `zpctl restart --group web`.
  Risk: looks like the start of a dependency graph; flagged as a
  guardrail concern.
- **`zpctl restart-after redis`**: restart redis and every service
  later in filename order. Useful when changing a base service whose
  config dependents pick up at startup.
- **`zpctl drain <service>`**: send the configured drain signal, wait,
  send the stop signal. Niche but cheap.
- **RSS-growth warnings**: zpinit periodically samples
  `/proc/<pid>/status` and logs `[zpinit] WARN service X rss grew
  64MB -> 512MB over 2h`. No alerting (no metrics endpoint), just
  observable in `docker logs`.
- **`zpctl complete bash|zsh`**: shell completion. Trivial polish,
  unrelated to mode 3 specifically.

## Mode 3 phase ordering (extension)

The boot sequence from the earlier phase-ordering proposal still
applies. Mode 3 extends it:

11. Mode dispatch (mode 3): start services in filename order; per
    service, run `pre_start` hook (if added), spawn, wait for
    `[ready]` to pass before starting the next.
12. Running phase: reap children, restart on crash with backoff,
    run `[live]` probes, capture audit events, accept zpctl
    commands, watch for SIGHUP / `zpctl update`.
13. Shutdown phase (signal received or `exit_code_from` triggered):
    in REVERSE filename order, per service:
    a. Run `pre_stop` hook with `pre_stop_timeout`.
    b. Walk `stop_signals` ladder, waiting per entry.
    c. Wait for terminal state, escalate to SIGKILL.

This sequence stays serial (per `CLAUDE.md`'s reverse-serial teardown
rule) so flush-on-shutdown semantics survive.

## Off-limits given current non-goals

Recorded so we don't relitigate later. From `CLAUDE.md`:

- **Health / metrics endpoint inside zpinit.** Collides with "no web UI /
  XML-RPC / metrics endpoint."
- **In-config env interpolation** (`${DB_HOST}` inside zpinit.toml itself).
  Collides with "no env interpolation in configs." External-file templating
  via `zpinit envsubst` is the workaround and stays out of the config.
- **Pipe-based log enrichment** (zpinit captures child stdout/stderr and
  rewraps it). Collides with the FD-inheritance-only rule. The boot banner
  shipped in v0.4.0 is fine because it writes to zpinit's own stderr pre-
  dispatch, not the child's stream.
- **Service dependency graphs** (anything with explicit `depends_on`
  edges, condition-based ordering beyond filename, or "wait until peer
  service emits an event"). Collides with "filename order + readiness
  probes only." Service groups (mid-tier in the mode-3 list) sit close
  to this line; they're a flat label, not a graph, but worth a check.
- **Interactive `zpctl fg`** (attach-and-watch a service's stdout in
  real-time). Explicit non-goal. `zpctl tail --follow` (shipped) is the
  closest variant: it streams a log file rather than the live process
  FD, which keeps the FD-inheritance-only rule intact.
- **Log rotation inside zpinit.** Use logrotate or stdout-to-host-logging
  (existing non-goal); affects items 17 and 23.

## Already shipped (kept here for the priority discussion only)

The following items from earlier drafts have landed and are no longer
in the "pending" section above:

- Item 4 boot banner (shipped v0.4.0).
- Item 18 `--dry-run` resolved plan (shipped v0.4.0 as `zpinit --plan`).
- Item 27 `zpctl ready` and `zpctl status --verbose` (shipped v0.4.0).
- Mid-tier item `zpctl tail --follow` (shipped v0.4.0 with a streaming
  control-protocol extension).

References below preserve their original numbering for traceability;
the items themselves were removed from the body of this file.

## Suggested priority if we move forward

The cluster with the best signal-to-cost ratio:

1. `[[wait]]` external preconditions
2. `[require]` assertions
3. `[runtime]` normalization
4. Swiss-army subcommands (was item 5)

Small, declarative, mode-agnostic, don't blur existing mode boundaries, and
turn mode 1 from "exec with config validation" into "the shortest path to
a well-behaved container." (6) and (7) are useful but more incremental.
(8) and (9) need explicit discussion before any commitment.

(10) Secrets management is in its own tier: high fleet value (one central
place for backend auth, redaction, injection) but substantial design
surface (backend plugin model, rotation semantics, redaction discipline).
A minimal v1 with just `file` and `command` backends would be cheap and
prove the plumbing; adding native Vault / AWS-SM / etc. is the part that
needs deliberate scoping.

Within the mode-2 cluster (items 11-19):

- (13) `[[render]]` templating: high value, low risk, kills the most
  common entrypoint.sh chore.
- (14) `zpinit env` subcommands: small, kills the `/run/zpinit/env`
  handwriting footguns.
- (16) per-script and per-phase timing envelope: cheap, fleet-uniform
  `docker logs` triage.

(11) `[[entrypoint]]` and (12) sidecar config are mid-tier: high
expressiveness, but they expand the schema meaningfully and need a clear
decision on phase order vs. `entrypoint.d/`. Probably ship after the
"easy cluster" so we have evidence for which knobs (timeout, user, once,
retries, when) get used most.

(15) multiple entrypoint dirs is mid-tier: small implementation, real
operator value, but only really matters once we have base images we
don't fully control.

(17) per-script log files is mid-tier: directly addresses the failure-
forensics gap, depends on whether item 16's envelope alone is enough.

(19) parallel within priority bands: defer indefinitely.

The phase-ordering section is not a feature; commit that ordering to
`docs/architecture.md` the first time any of phases 3-10 ships.

Within the mode-3 cluster (items 20-29):

- (20) `[live]` probes: high value, materially closes the
  "deadlocked but not crashed" gap. Probably the single biggest
  reliability win for shop runtimes. Reuses the shared probe package
  that items 1 and `[ready]` define, so cost is mostly schema and
  scheduling.
- (21) `[limits]`: high value where containers run multiple services
  with unequal trust. Pure rlimit + oom_score_adj is small-surface;
  cgroup support is a later phase.
- (23) audit / crashlog: high value for operators, low risk, builds
  on the timing-envelope work from item 16 (the boot banner shipped
  separately in v0.4.0).
- (26) multi-signal stop ladder: medium value, fits naturally into
  the existing reverse-serial shutdown.

(22) pre-stop hook is the most "philosophical" addition: it expands the
service contract beyond "command + lifecycle" toward a richer hook
model. Worth doing but design carefully so it doesn't grow into
post_start, on_reload, on_signal, etc., which would be dependency-graph-
adjacent.

(24) configurable backoff and (25) smart reload diffing are refinements
of existing behavior; ship alongside the items that motivate them
(`[on_crash]` motivates configurable backoff, `[live]` config motivates
smarter reload diff).

(28) better `[ready]` failure diagnostics is pure operator UX; cheap to
ship at any point.

(29) `[on_crash]` is high-value but couples to (24) backoff config and
to the in-config-interpolation discussion. Best shipped after both are
settled.

Mid-tier mode-3 items: ship opportunistically; none are blockers.
