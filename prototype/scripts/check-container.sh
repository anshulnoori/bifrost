#!/usr/bin/env bash
# Offline image checks. Never enroll a node; the fake key has no network access.
set -euo pipefail
image=${1:-tailnet-proof:local}
name="tailnet-proof-test-$$"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
test "$(docker image inspect "$image" --format '{{.Architecture}}')" = amd64
test "$(docker image inspect "$image" --format '{{.Config.User}}')" = 65532:65532
if docker run --rm --network none --cap-drop ALL "$image" >/tmp/proof-unconfigured-$$.log 2>&1; then
  echo 'FAIL: image should require enrollment configuration'; exit 1
fi
test "$(cat /tmp/proof-unconfigured-$$.log)" = enrollment_unconfigured
rm /tmp/proof-unconfigured-$$.log
docker create --name "$name" --network none --cap-drop ALL --security-opt no-new-privileges \
  -e TS_AUTHKEY=invalid-offline-test-key "$image" >/dev/null
docker start "$name" >/dev/null
sleep 3
docker exec "$name" python -c 'import urllib.request; assert urllib.request.urlopen("http://127.0.0.1:8080/healthz").status == 200'
docker exec "$name" sh -c 'test ! -e /dev/net/tun'
docker stop --time 15 "$name" >/dev/null
test "$(docker inspect "$name" --format '{{.State.ExitCode}}')" = 0
test "$(docker logs "$name" 2>&1)" = supervisor_stopped
echo 'PASS: amd64, unprivileged, no TUN/capabilities, fail-closed enrollment, loopback HTTP, SIGTERM, no raw auth logs'
