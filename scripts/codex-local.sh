#!/usr/bin/env bash
# Local-only deployment. Credentials stay in a private directory across restarts.
set -euo pipefail
umask 077
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
if [[ $# -gt 1 || ( $# -eq 1 && $1 != --prepare-only ) ]]; then
  echo 'Usage: bash scripts/codex-local.sh [--prepare-only]' >&2
  exit 2
fi
for tool in jq openssl flock; do
  command -v "$tool" >/dev/null || { echo "Required tool: $tool" >&2; exit 1; }
done
data=${CODEX_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/bifrost-codex}
mkdir -p "$data"
data=$(cd "$data" && pwd)
chmod 700 "$data"
exec 9>"$data/.lock"
flock -n 9 || { echo 'This local Codex deployment is already running or being prepared.' >&2; exit 1; }
if [[ ( -f "$data/config.json" || -f "$data/config.db" ) && ! -f "$data/credentials.env" ]]; then
  echo 'Existing configuration has no credentials.env. Restore its original encryption secret; do not regenerate it.' >&2
  exit 1
fi
if [[ ! -f "$data/credentials.env" ]]; then
  credentials=$(mktemp "$data/credentials.env.XXXXXX")
  trap 'rm -f "${credentials:-}"' EXIT
  {
    printf 'export BIFROST_ENCRYPTION_KEY=%s\n' "$(openssl rand -hex 32)"
    printf 'export CODEX_GATEWAY_KEY=sk-bf-%s\n' "$(openssl rand -hex 32)"
    printf 'export CODEX_ADMIN_PASSWORD=%s\n' "$(openssl rand -hex 32)"
  } > "$credentials"
  mv "$credentials" "$data/credentials.env"
  trap - EXIT
fi
chmod 600 "$data/credentials.env"
# This is a locally generated, owner-only shell file, never an OAuth token import.
source "$data/credentials.env"
: "${BIFROST_ENCRYPTION_KEY:?Missing encryption secret}"
: "${CODEX_GATEWAY_KEY:?Missing gateway key}"
: "${CODEX_ADMIN_PASSWORD:?Missing dashboard password}"
if [[ ! -f "$data/config.json" ]]; then
  config=$(mktemp "$data/config.json.XXXXXX")
  trap 'rm -f "${config:-}"' EXIT
  jq --arg db "$data/config.db" '
    del(."$schema") |
    .config_store.config.path = $db |
    .governance.auth_config = {is_enabled: true, admin_username: "codex", admin_password: "env.CODEX_ADMIN_PASSWORD"}
  ' "$root/tests/codex/config.example.json" > "$config"
  mv "$config" "$data/config.json"
  trap - EXIT
fi
printf 'Private deployment directory: %s\n' "$data"
printf 'Dashboard username: codex. Password and gateway key are in credentials.env in that directory. Do not share this file.\n'
[[ ${1:-} == --prepare-only ]] && exit 0
binary=${BIFROST_BINARY:-$data/bifrost}
if [[ -z ${BIFROST_BINARY:-} ]]; then
  cd "$root"
  [[ -f go.work ]] || make setup-workspace
  [[ -d ui/node_modules ]] || npm --prefix ui ci
  npm --prefix ui run build
  go build -o "$binary" ./transports/bifrost-http
fi
[[ -x "$binary" ]] || { echo 'BIFROST_BINARY must name an executable gateway.' >&2; exit 1; }
echo 'Starting a loopback-only gateway. Open its dashboard, sign in, then select Providers → Codex.'
exec "$binary" -app-dir "$data" -host 127.0.0.1 -port "${CODEX_PORT:-8080}"
