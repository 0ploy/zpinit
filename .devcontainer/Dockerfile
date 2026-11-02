# Dev container for zpinit. Setup and rationale: README.md.
# Go must satisfy the `go` directive in go.mod (1.26). The image already ships
# gcc, which `go test -race` needs for cgo. Digest-pinned; bump via
# `docker buildx imagetools inspect golang:1.26-bookworm`.
FROM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c

# jq: bootstrap.sh merges the Claude settings file. Sudo is limited to the two
# chown calls the bootstrap needs to take ownership of the named volumes.
RUN apt-get update \
 && apt-get install -y --no-install-recommends fish jq sudo vim \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -u 1000 -s /usr/bin/fish dev \
 && printf '%s\n' \
      'dev ALL=(root) NOPASSWD: /usr/bin/chown dev\:dev /workspaces/zpinit' \
      'dev ALL=(root) NOPASSWD: /usr/bin/chown -R dev\:dev /home/dev/go /home/dev/.cache/go-build /home/dev/.claude' \
      > /etc/sudoers.d/dev \
 && chmod 0440 /etc/sudoers.d/dev \
 && visudo -c -f /etc/sudoers.d/dev

# Per-user Go dirs; the compose volumes mount into these. GOTOOLCHAIN=local
# stops go from silently downloading a newer toolchain when go.mod moves:
# bump the base image instead, so the container matches CI.
ENV GOPATH=/home/dev/go \
    GOCACHE=/home/dev/.cache/go-build \
    GOTOOLCHAIN=local \
    PATH="/home/dev/go/bin:/home/dev/.local/bin:${PATH}"
# Debian's /etc/profile overwrites PATH in login shells (`bash -l`, editor
# terminals), dropping the image's Go dir. Fish inherits the ENV above as is.
RUN printf '%s\n' \
      'export PATH="/home/dev/go/bin:/home/dev/.local/bin:/usr/local/go/bin:$PATH"' \
      > /etc/profile.d/go.sh

# GitHub CLI, checksum-verified against the release's checksums.txt, so a
# version bump is the only edit needed. Auth is GH_TOKEN (docker-compose.yml);
# gh is also git's credential helper, so it is on the push path.
ARG GH_VERSION=2.102.0
RUN set -eu; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in amd64|arm64) ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac; \
    base="https://github.com/cli/cli/releases/download/v${GH_VERSION}"; \
    tmp="$(mktemp -d)"; cd "$tmp"; \
    curl -fsSLO "${base}/gh_${GH_VERSION}_linux_${arch}.tar.gz"; \
    curl -fsSLO "${base}/gh_${GH_VERSION}_checksums.txt"; \
    sha256sum --check --ignore-missing "gh_${GH_VERSION}_checksums.txt"; \
    tar -xzf "gh_${GH_VERSION}_linux_${arch}.tar.gz"; \
    install -m 0755 "gh_${GH_VERSION}_linux_${arch}/bin/gh" /usr/local/bin/gh; \
    cd /; rm -rf "$tmp"; \
    gh --version

# Claude Code, native installer (no Node runtime). Its state lives in
# CLAUDE_CONFIG_DIR (docker-compose.yml), a named volume, so the login
# survives rebuilds.
RUN su - dev -s /bin/bash -c 'curl -fsSL https://claude.ai/install.sh | bash'

# Bootstrap, baked in so it is available before the workspace is mounted and
# can be re-run by hand.
COPY bootstrap.sh /usr/local/bin/zpinit-bootstrap
RUN chmod 0755 /usr/local/bin/zpinit-bootstrap

# Mountpoints, created dev-owned: a fresh volume inherits the ownership of the
# directory it mounts over, and a root-owned ~/.config breaks fish and the Zed
# remote server.
RUN install -d -o dev -g dev /workspaces/zpinit /home/dev/go/pkg/mod \
      /home/dev/.cache/go-build /home/dev/.config /home/dev/.claude

# Compare only mtime+size for change detection. ctime/inode are unstable on the
# virtiofs bind mount and would make git report the whole tree as modified.
RUN git config --system core.checkStat minimal

USER dev
