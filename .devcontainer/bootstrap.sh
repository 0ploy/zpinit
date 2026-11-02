#!/usr/bin/env bash
# Devcontainer bootstrap, baked into the image as zpinit-bootstrap and run on
# create and on every start (devcontainer.json). Idempotent: re-run it any time.
set -euo pipefail

WS=/workspaces/zpinit
problems=0
err() { echo "✗ $*" >&2; problems=$((problems + 1)); }

# 1. Per-developer prerequisites, from .devcontainer/.env on the host (see
#    .env.example) and the mounted ~/.gitconfig.
[ -n "${GH_TOKEN:-}" ] || err "GH_TOKEN is empty: set it in .devcontainer/.env and restart the container."
[ -n "${GIT_CONFIG_VALUE_0:-}" ] || err "GIT_EMAIL is empty: set it in .devcontainer/.env and restart the container."
git config user.name >/dev/null 2>&1 || err "git user.name is not set in the host ~/.gitconfig."
if [ "$problems" -gt 0 ]; then
  echo "$problems problem(s) above; git push and gh will not work until fixed." >&2
fi

# 2. Own the named volumes. Docker creates the mountpoints as root, and the
#    contents may belong to another UID: some editors remap dev to the host UID
#    on macOS (Zed does, the bare devcontainer CLI does not). Both argument
#    lists must match the sudoers entries in the Dockerfile exactly. `|| true`
#    for the bind-mounted workspace, where chown may fail.
sudo chown dev:dev "$WS" || true
sudo chown -R dev:dev /home/dev/go /home/dev/.cache/go-build /home/dev/.claude

# 3. Dependencies (only BurntSushi/toml today; cheap, and warms the volume).
cd "$WS"
go mod download

# 4. Bypass permissions in this sandbox. Merged, not written: Claude creates
#    settings.json itself at first login.
settings=/home/dev/.claude/settings.json
[ -s "$settings" ] && jq -e . "$settings" >/dev/null 2>&1 || echo '{}' > "$settings"
jq '.permissions.defaultMode = "bypassPermissions"
    | .skipDangerousModePermissionPrompt = true' "$settings" > "$settings.tmp" \
 && mv "$settings.tmp" "$settings"

echo "zpinit devcontainer bootstrap complete."
echo "Build: make build; tests: go test -race ./... and make integration (Linux paths run here)."
