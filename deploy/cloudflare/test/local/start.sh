#!/usr/bin/env bash
# Starts only loopback local emulators. Never runs login, deploy, or remote mode.
set -euo pipefail
root=$(realpath "$(dirname "$0")/../..")
mode=${1:-stock}
[[ $mode == stock || $mode == --ingress-fixture ]] || exit 2
docker network inspect bridge >/dev/null
home=$(mktemp -d /tmp/bifrost-wrangler-home.XXXXXX)
mkdir -p "$home/.docker/cli-plugins"
# Copy the executable, not Docker config or credentials, into the isolated HOME.
if [[ -f "$HOME/.docker/cli-plugins/docker-buildx" ]]; then
  cp "$HOME/.docker/cli-plugins/docker-buildx" "$home/.docker/cli-plugins/"
fi
extra=''
if [[ $mode == --ingress-fixture ]]; then
  if ! docker container inspect bifrost-local-registry >/dev/null 2>&1; then
    docker run -d --name bifrost-local-registry --network host -e REGISTRY_HTTP_ADDR=127.0.0.1:5000 \
      registry:3.0.0@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e
  else
    docker start bifrost-local-registry >/dev/null
  fi
  docker build --network host -f "$root/test/local/Ingress.Dockerfile" -t 127.0.0.1:5000/bifrost-local-ingress:fixture "$root/test/local"
  # This registry is loopback in the orb, not an external publication.
  docker push 127.0.0.1:5000/bifrost-local-ingress:fixture
  extra='MINIFLARE_CONTAINER_EGRESS_IMAGE=127.0.0.1:5000/bifrost-local-ingress:fixture'
fi
for name in inference admin control; do amp orb service stop "wrangler-$name" >/dev/null 2>&1 || true; done
node "$root/test/local/prepare.mjs"
printf '%s\n' "$home" > "$root/.wrangler/local-e2e/runtime-home"
for name in inference admin control; do
  amp orb service start "wrangler-$name" --command "env -i PATH='$PATH' HOME='$home' WRANGLER_SEND_METRICS=false DOCKER_HOST=unix:///var/run/docker.sock $extra node '$root/node_modules/wrangler/bin/wrangler.js' dev --local --config '$root/.wrangler/local-e2e/$name.json' --persist-to '/tmp/bifrost-wrangler-$name-state' --show-interactive-dev-session=false"
done
for attempt in {1..60}; do
  if curl --fail --silent --max-time 1 http://127.0.0.1:8793/status >/dev/null; then
    echo 'Local Wrangler bindings ready. No portal or remote endpoint was created.'
    exit 0
  fi
  sleep 1
done
echo 'Local startup failed. Inspect: amp orb service logs wrangler-inference' >&2
exit 1
