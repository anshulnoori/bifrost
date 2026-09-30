#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
export CODEX_DATA_DIR="$scratch/deployment"
bash "$root/scripts/codex-local.sh" --prepare-only > "$scratch/output"
test "$(stat -c %a "$CODEX_DATA_DIR/credentials.env")" = 600
test "$(stat -c %a "$CODEX_DATA_DIR/config.json")" = 600
jq -e '.governance.auth_config.is_enabled == true and .client.enforce_auth_on_inference == true and .encryption_key == "env.BIFROST_ENCRYPTION_KEY"' "$CODEX_DATA_DIR/config.json" >/dev/null
cp "$CODEX_DATA_DIR/credentials.env" "$scratch/original"
bash "$root/scripts/codex-local.sh" --prepare-only >> "$scratch/output"
cmp "$scratch/original" "$CODEX_DATA_DIR/credentials.env"
source "$CODEX_DATA_DIR/credentials.env"
for secret in "$BIFROST_ENCRYPTION_KEY" "$CODEX_GATEWAY_KEY" "$CODEX_ADMIN_PASSWORD"; do
  if rg -Fq -- "$secret" "$scratch/output"; then echo 'Launcher leaked a credential' >&2; exit 1; fi
done
mv "$CODEX_DATA_DIR/credentials.env" "$scratch/removed"
if bash "$root/scripts/codex-local.sh" --prepare-only >> "$scratch/output" 2>&1; then
  echo 'Launcher regenerated an encryption secret for an existing deployment' >&2
  exit 1
fi
rm "$CODEX_DATA_DIR/config.json"
touch "$CODEX_DATA_DIR/config.db"
if bash "$root/scripts/codex-local.sh" --prepare-only >> "$scratch/output" 2>&1; then
  echo 'Launcher regenerated an encryption secret for an orphaned database' >&2
  exit 1
fi
echo 'PASS: private defaults, restart persistence, no credential output, and lost-secret guard'
