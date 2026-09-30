#!/usr/bin/env bash
# Disposable local services only. Never point this suite at a shared cache.
set -euo pipefail
: "${BIFROST_PACKAGE:?Build nix .#bifrost-stack and set BIFROST_PACKAGE to its output}"
case "$(uname -m)" in
  x86_64) digest=203692cbb7d59887cd7723f88cefa0c470d74037e3f82024b17cfac31345d4f5 ;;
  aarch64) digest=cc16e0c672ffdfdbee4581e146d41086f408468489e21b8625f877a332b998ba ;;
  *) echo 'Unsupported validation architecture' >&2; exit 1 ;;
esac
engine="${CONTAINER_ENGINE:-docker}"
name="bifrost-cache-test-$$"
trap '"$engine" rm -f "$name" >/dev/null 2>&1 || true' EXIT
if [[ ${engine##*/} == podman ]]; then
  data_mount=(--mount=type=tmpfs,destination=/data,tmpfs-mode=0700,U=true)
else
  data_mount=(--tmpfs /data:uid=999,gid=999,mode=700)
fi
"$engine" run -d --name "$name" --user 999:999 --read-only --cap-drop ALL \
  --security-opt no-new-privileges --memory 3g "${data_mount[@]}" \
  -p 127.0.0.1::6379 --entrypoint valkey-server "valkey/valkey-bundle@sha256:$digest" \
  --loadmodule /usr/lib/valkey/libsearch.so --bind 0.0.0.0 \
  --requirepass synthetic-local-only --save '' --appendonly no \
  --maxmemory 2gb --maxmemory-policy allkeys-lfu >/dev/null
"$engine" exec "$name" sh -c 'test "$(stat -c "%u:%g:%a" /data)" = "999:999:700"'
export REDIS_ADDR
REDIS_ADDR=$("$engine" port "$name" 6379/tcp)
export REDIS_PASSWORD=synthetic-local-only
ready=false
for attempt in {1..30}; do
  if "$engine" exec -e VALKEYCLI_AUTH="$REDIS_PASSWORD" "$name" valkey-cli -e FT._LIST >/dev/null 2>&1; then
    ready=true; break
  fi
  sleep 1
done
"$ready"
go test -race ./framework/vectorstore -run '^Test(VectorDimensionFromFTInfo|RedisStore_(Integration|FilteringScenarios|VectorSearch|CreateNamespaceRejectsDimensionChange|NamespaceDimensionHandling))$' -count=1
BIFROST_TEST_VALKEY=1 go test -race ./plugins/semanticcache -run '^TestVirtualKeyCache' -count=2
VALKEY_TEST_ADDR="$REDIS_ADDR" node --test deploy/nixos/package.test.mjs
BIFROST_TEST_SEMANTIC=1 VALKEY_TEST_ADDR="$REDIS_ADDR" node --test deploy/nixos/package.test.mjs
