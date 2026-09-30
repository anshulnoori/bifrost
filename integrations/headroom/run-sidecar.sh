#!/bin/sh
set -eu
: "${HEADROOM_PROXY_TOKEN:?Set a private sidecar token of at least 32 bytes}"
[ "${#HEADROOM_PROXY_TOKEN}" -ge 32 ] || exit 1
export HEADROOM_BEACON="${HEADROOM_BEACON:-off}"
export HEADROOM_TELEMETRY="${HEADROOM_TELEMETRY:-off}"
export HEADROOM_LOG_PAYLOAD_PREVIEW=0
export HEADROOM_CCR_BACKEND=memory
exec headroom proxy --host 127.0.0.1 --port 8787 --stateless --no-cache --no-ccr "$@"
