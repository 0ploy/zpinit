# zpinit Dev Container

Compose-based dev environment for zpinit (Go 1.26). The point of running it
in a container at all: zpinit's hot paths (`Pdeathsig`, `/proc`, inotify on
cgroupfs) are Linux-only, so `make integration` and the `//go:build linux`
tests run here but not on the Mac.

## Setup

1. Copy the per-developer env file and fill it in:

   ```sh
   cp .devcontainer/.env.example .devcontainer/.env
   chmod 600 .devcontainer/.env
   ```

   `GH_TOKEN` is a fine-grained PAT on `0ploy/zpinit` (Contents read/write,
   Pull requests read/write). It authenticates `gh` and, via
   `gh auth git-credential`, git itself. Every `git@github.com:` remote is
   rewritten to HTTPS inside the container. `GIT_EMAIL` is your commit email.

2. Make sure the host `~/.gitconfig` has `user.name`; it is bind-mounted
   read-only. Commits made in the container are not signed.

Then open the folder in a dev-container-capable editor and reopen in the
container. The bootstrap runs on create and on every start and reports
anything missing from the steps above. If your editor skips lifecycle hooks,
run it yourself:

```sh
zpinit-bootstrap            # go mod download; idempotent
make build                  # bin/zpinit, bin/zpctl
go test -race -count=1 ./...
make integration            # Linux-only full-binary tests, same as CI
```

## Layout

- **Host checkout is bind-mounted** at `/workspaces/zpinit`, edits land on the
  host. `core.checkStat=minimal` in the image keeps git from flagging the
  whole tree on virtiofs.
- **Go caches on named volumes** (`zpinit-gomod`, `zpinit-gocache`) so builds
  stay fast and Linux artifacts never touch the bind mount. Nothing shadows a
  host directory: `bin/` and the cross-compiled release binaries are written
  into the checkout and are git-ignored.
- **Claude Code** via the native installer; its store is the `zpinit-claude`
  volume (`CLAUDE_CONFIG_DIR`), separate from the host's `~/.claude`. The
  bootstrap sets `bypassPermissions` for this sandbox.
- **No docker socket.** The release `Dockerfile` and the `docker run` recipes
  in `docs/development.md` are host-side; inside the container you are
  already on Linux, so run the tests directly.
- **Pinned:** the Go base image by digest, `gh` by version (checksum-verified
  at build). Bump the digest when `go.mod` moves; `GOTOOLCHAIN=local` makes a
  mismatch fail loudly instead of downloading a toolchain behind your back.

## Troubleshooting

- **Bootstrap reports a missing token or email**: fix `.devcontainer/.env`,
  then restart the container (Compose reads `.env` at container start).
- **git asks for a username or 403s on push**: token missing, expired or
  without Contents write. `gh auth status` tells which.
- **`go: go.mod requires go >= 1.x (running go 1.26; GOTOOLCHAIN=local)`**:
  bump the base image digest in the Dockerfile and rebuild.
- **Config change not picked up (Zed)**: Zed does not rebuild the container
  automatically. `docker compose -f .devcontainer/docker-compose.yml down`,
  then reopen in the dev container.
- **Clean rebuild**: `docker compose -f .devcontainer/docker-compose.yml down -v`
  discards all volumes, including the Go caches and the Claude Code login.
  `down` without `-v` keeps them.
