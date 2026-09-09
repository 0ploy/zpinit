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
make test        # unit tests (Linux + macOS)
make integration # full-binary integration tests (Linux only)
make lint        # gofmt + go vet
```

Tests use the standard library only. No testify, gomock, or similar.
The only approved external dependency is `github.com/BurntSushi/toml`;
anything else needs explicit approval before `go get`.

CI runs unit tests on every push and PR (Linux + macOS), then
integration tests (Linux) once unit passes; there is no branch
filter. Both run with `go test -race -count=1` so the
data-race class the v0.3.x review surfaced can't regress unnoticed
in the supervise / reload / autoscale fan-out.

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
