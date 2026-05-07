# Development

## Build

```sh
make build       # static binaries to bin/zpinit and bin/zpctl
```

CGO is disabled and the build uses `-trimpath` plus `-s -w`, producing
a ~3 MB static binary. The version string is set from
`git describe --tags --always --dirty`.

## Test

```sh
make test        # unit tests (Linux + macOS)
make integration # full-binary integration tests (Linux only)
make lint        # gofmt + go vet
```

Tests use the standard library only. No testify, gomock, or similar.
The only approved external dependency is `github.com/BurntSushi/toml`;
anything else needs explicit approval before `go get`.

CI runs unit on every push (Linux + macOS), integration on PRs and
pushes to main.

## Linux-only paths

zpinit uses `Pdeathsig`, `Setpgid`, and `/proc` in its hot paths.
macOS builds compile via build tags but don't exercise PID-1 paths.
Run `make integration` on a Linux box (or in a container) to validate
those code paths.

## Project notes for agents

`CLAUDE.md` in the repo root documents the load-bearing design rules
and gotchas that don't fall out of reading the source. Update it when
a non-obvious invariant changes; keep it short.

## Implementation history

The implementation history is in `git log --oneline`. Each phase is
one commit with a detailed message explaining what landed and why.
Per-phase rationale lives in commit history; the docs cover stable
state.
