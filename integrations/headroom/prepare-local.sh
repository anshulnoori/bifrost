#!/usr/bin/env bash
# Prepare the private sidecar and its gateway environment in a disposable orb.
# Does not change provider credentials or restart the gateway.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
runtime=${HEADROOM_RUNTIME_DIR:-/tmp/bifrost-headroom-runtime}
umask 077
mkdir -p "$runtime"
if [[ ! -f "$runtime/environment" ]]; then
  {
    printf 'export HEADROOM_PROXY_TOKEN=%s\n' "$(openssl rand -hex 32)"
    printf 'export HEADROOM_SCOPE_KEY=%s\n' "$(openssl rand -hex 32)"
    printf 'export HEADROOM_METRICS_TOKEN=%s\n' "$(openssl rand -hex 32)"
  } > "$runtime/environment"
fi
chmod 600 "$runtime/environment"
uv venv --allow-existing "$runtime/venv" --python 3.11
uv pip install --python "$runtime/venv/bin/python" --require-hashes -r "$root/integrations/headroom/requirements.lock"
amp orb service start headroom --port 8787 --command "source '$runtime/environment'; export PATH='$runtime/venv/bin':\$PATH; bash '$root/integrations/headroom/run-sidecar.sh'"
printf 'Sidecar prepared. Gateway command must source %s/environment before starting.\n' "$runtime"
