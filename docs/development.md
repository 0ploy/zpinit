# Development

## Build

```sh
make build       # static binaries to bin/zpinit and bin/zpctl
```

CGO is disabled and the build uses `-trimpath` plus `-s -w`, producing
a ~3 MB static binary. The version string is set from
`git describe --tags --always --dirty`.

Requires Go >= 1.26 (the `go` directive in `go.mod`). CI and the
release pipeline both install it via `setup-go` with
`go-version-file: go.mod`, so bumping that one line moves every build.
The runtime floor matters: zpinit relies on the Go 1.25+ container-
aware `GOMAXPROCS` (its own scheduler tracks the cgroup CPU quota
instead of the host core count), and the test suite uses
`testing/synctest` (1.25+) for deterministic virtual-time concurrency
tests.

## Test

```sh
make test             # fast dev loop: unit tests, cached (Linux + macOS)
make test-race        # unit tests as CI runs them: -race -shuffle=on -count=1
make integration      # full-binary integration tests (Linux only)
make integration-race # the CI variant of the above
make coverage         # merged unit + integration coverage -> cover/coverage.html
make lint             # gofmt -s, go vet (incl. integration tag), staticcheck
make vuln             # govulncheck against the module
make check            # everything CI gates on: lint vuln test-race integration-race
```

Tests use the standard library only. No testify, gomock, or similar.
The only approved external dependency is `github.com/BurntSushi/toml`;
anything else needs explicit approval before `go get`. `staticcheck`
and `govulncheck` run through `go run pkg@version` (versions pinned in
the Makefile), which uses the module cache and never touches `go.mod`.

CI runs the same Makefile targets: `make lint` + `make vuln` (lint
job), `make test-race` on Linux and macOS (unit job), then
`make integration-race` and `make coverage` once unit passes. It
triggers on pull requests and on pushes to `main` or a release tag, so
a branch with an open PR gets one run per push, not two. `-race` exists because the v0.3.x review surfaced a
data-race class in the supervise / reload / autoscale fan-out;
`-shuffle=on` exists so a test that only passes in file order fails
loudly; `-count=1` keeps a flake from hiding behind a cached pass.
Coverage is reported (job summary + artifact), not gated: a threshold
would only invite padding tests.

### Coverage

`make coverage` produces one merged figure: unit test binaries write
binary coverage data through `-test.gocoverdir`, and the integration
`TestMain` builds instrumented `zpinit` / `zpctl` binaries when
`ZPINIT_TEST_COVERDIR` is set, pointing their `GOCOVERDIR` at the same
directory. `go tool covdata` merges all of it. The per-package `go test
-cover` lines printed during the unit phase are unit-only and look
low for `cmd/zpinit` (it is exercised through the real binary); the
"per package" block at the end is the real number.

### Writing tests against the fake runner

Runner state changes are observable before the matching signal lands:
`handleStop` transitions to Stopping, then calls `SignalGroup`. A test
that has just seen a state and wants the signal must use
`waitSignals(t, p, n, timeout)` from `fake_test.go`, never read
`signalsReceived()` directly after `waitState`; the bare read passed
by luck and failed under `-count=3`.

### Integration test conventions

- One supervisor per test, config in `t.TempDir()`, socket path set
  through `control_socket` in `zpinit.toml`. `startZpinit` /
  `zpctlRunner` / `waitForRunning` / `waitForStateRow` in
  `reload_test.go` and `lifecycle_test.go` are the shared helpers.
- Assert what the product contract promises, not what the scheduler
  usually does. Filename order fixes spawn order only; a test that
  needs "B runs after A is up" gives A a `[ready]` probe.
- Without a `[ready]` probe a service is booted the moment it is
  spawned, so an immediate exit is a normal EXITED, not a boot
  failure. Boot-failure tests give the service a probe that never
  passes and let the child die while it is polling.
- A zpinit that a test must observe exiting goes through `watchExit`;
  a bare `exited <- zp.Wait()` channel read twice (test body, then
  cleanup) hangs the suite until the `go test` timeout.
- `TestPID1InContainer` is the only test that runs zpinit as a real
  PID 1 (orphan reaping, container exit code). It needs a docker
  daemon and skips without one, so it runs on the ubuntu CI runner,
  not in a docker-less dev container.

## Linux-only paths

zpinit uses `Pdeathsig`, `Setpgid`, and `/proc` in its hot paths.
macOS builds compile via build tags but don't exercise PID-1 paths.
Run `make integration` on a Linux box (or in a container) to validate
those code paths.

The resource watcher's inotify trigger (`internal/resources/
notify_linux.go`) is one of these: off Linux `newCgroupNotify` returns
nil and the watcher falls back to its backstop poll, so
`notify_linux_test.go` is `//go:build linux` and never runs on a macOS
`make test`. To exercise it without a Linux box:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o /tmp/res.test ./internal/resources
docker run --rm -v /tmp/res.test:/t:ro alpine:3.20 /t -test.v
```

The `-race` build needs cgo, so for a race-checked Linux run mount the
tree into a Go image instead:

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.26 go test -race ./...
```

## Project notes for agents

`CLAUDE.md` in the repo root documents the load-bearing design rules
and gotchas that don't fall out of reading the source. Update it when
a non-obvious invariant changes; keep it short.

## Implementation history

The implementation history is in `git log --oneline`. Each phase is
one commit with a detailed message explaining what landed and why.
Per-phase rationale lives in commit history; the docs cover stable
state.
