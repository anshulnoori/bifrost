# Local Wrangler validation

This runs actual Wrangler CLI dev servers, workerd, Durable Objects, service bindings,
and Docker processes. It does not deploy to Cloudflare or load real credentials.
The public deployment configurations remain route-free and disabled.

## What runs

```text
HTTP test ──▶ inference Worker ──▶ Container DO / SDK ──▶ Docker mock application
HTTP test ──▶ admin Worker ──▶ private AdminOrigin binding ────────────┘
Loopback test controller ──▶ same DO: status, idle check, stop, readiness fault
```

The local entrypoint subclasses the production Container class only to expose fixture RPCs.
Production fetch, admission, stream wrapping, and lifecycle methods are unchanged.
Local admin uses the production entrypoint. Config generation copies the production
configs, changes names/origins/image, and injects synthetic values. All servers bind loopback.
The controller is never included in a production config, binding, or public route.

Only JWKS retrieval is substituted with a generated RSA public key. Signature verification,
issuer/audience/expiry/owner checks, and both admin verification layers execute normally.
The signing key stays in ignored scratch with mode 0600. It has no authority outside these
fixtures. The upstream application mocks Bifrost session/VK checks and provider responses.
Separate tests exercise the actual Bifrost bridge, Modal ASGI facade, and local PostgreSQL.
This is layered integration coverage, **not one real Bifrost→Modal→Neon cloud request**.

## Stock Container transport cannot run on this orb kernel

The stock local run was attempted first. Docker initially lacked its default bridge;
restarting the disposable daemon with a bridge resolved that problem. The stock
`cloudflare/proxy-everything` sidecar then failed with:

```text
Extension socket revision 0 not supported, missing kernel module?
iptables ... RULE_APPEND failed ... PREROUTING
```

This kernel lacks the required socket match and has no installed kernel modules. The
transparent proxy cannot initialize. Installing another user-space tool does not supply
the missing kernel feature. Wrangler offers no supported
"Containers enabled, transparent egress disabled" mode. Its JSON schema advertises an
object-form engine configuration that this pinned CLI rejects, so that path was not used.

The explicit `--ingress-fixture` option uses Miniflare's interceptor-image override. It
substitutes a restricted HTTP CONNECT transport for loopback ports 8080 and 6553 only.
It implements the startup control handshake but **does not emulate egress, DNS, or TLS
interception**. The application fixture performs no outbound requests. A public CA certificate
from Node's built-in trust list satisfies the unused CA handshake; no private CA key exists.
The image is served from a registry bound to orb loopback, not pushed to a remote registry.
Do not use this transport with production code or real credentials.

On a kernel with the required modules, omit `--ingress-fixture` to use stock transport.
Passing results on this orb used the explicit fixture; stock transport remains unverified.

## Local hosts without orb services

The stock transport supports foreground process supervision:

```sh
bash deploy/cloudflare/test/local/start.sh stock --process
# In a second terminal, with the same isolated toolchain:
node deploy/cloudflare/test/local/run.mjs
```

The supervisor refuses occupied ports before it generates credentials. It starts three
Wrangler processes with a cleared environment, fresh HOME, and private cache/state directories.
It preserves `PATH`, `DOCKER_HOST`, `TMPDIR`, and `XDG_RUNTIME_DIR` for portable tools and rootless engines.
The default Docker socket is `/var/run/docker.sock`. Logs and state stay under the printed
`.wrangler/local-e2e/process-*` directory. SIGINT or SIGTERM stops the child process groups.
An unexpected child exit stops the other children and returns failure.

The existing orb command remains unchanged. Process supervision supports stock transport only.
It does not install tools, configure an engine, or remove engine resources. After shutdown,
inspect the engine for remaining containers and remove only resources from this run.
Stop local servers before `npm test`: the supervisor regression tests use the same loopback ports.

### t1 stock run: transport works, restart fails

The 2026-09-23 t1 run used NixOS 26.11, kernel `7.2.4-cachyos-lto`, and rootless Podman 5.8.7.
The host `docker` command was a Podman alias, not Docker Engine. Test storage and the API
socket stayed in a private temporary root. No existing container storage or services changed.
Go 1.27.0 and Python 3.11.15 stayed in that root. Node 24.20.0 came from the existing Nix store.
The npm lock supplied Wrangler 4.136.3 and Containers SDK 0.3.7.

A test-local CLI wrapper removed `--provenance=false` and copied the stdin Dockerfile to a temporary file.
Podman rejected that Docker flag and could not reopen Node's stdin socket through `/dev/stdin`.
An isolated registry configuration resolved short image names through `docker.io`.
These changes did not alter the fixture Dockerfile, application, or stock sidecar image.
This run is not Docker Engine certification.

The stock image digest was
`sha256:0ef6716c52430096900b150d84a3302057d6cd2319dae7987128c85d0733e3c8`.
The kernel had socket modules on disk and automatically loaded them during the container run.
No explicit module-management command ran. No ingress-only substitute ran on t1.
Workerd reported a rootless gateway-bind fallback to loopback. The fixture made no outbound requests,
so stock image startup does not prove egress DNS or TLS interception.

The unchanged HTTP suite passed its first five groups, including JWT crypto, SSE bytes,
upstream cancellation, and active-stream idle protection. Group six failed on restart:

```text
Create container failed with [500] ... the container name ... is already in use
503 !== 200
```

The failure reproduced. Diagnostic copies with one-second and twelve-second waits after
the stopped state also failed. Those copies were removed. The original test assertion remains unchanged.
The full seven-group suite did not pass. A separate readiness-fault request returned 503
with zero leases, but the full suite did not reach group seven. The precise SDK/workerd/Podman
compatibility cause remains unresolved. A supported Docker Engine rerun remains a local validation gate.

The original 12 Node tests, TypeScript check, and both Worker dry-runs passed on t1.
The additional supervisor tests cover collisions, environment isolation, SIGINT, SIGTERM, and child failure.
The PostgreSQL migration/restricted-role test and Codex/Headroom race tests passed against disposable PostgreSQL 17.6.
The Python mock suite passed five tests and skipped its optional official compressor test.
No provider credentials, remote deployment, paid harness, or Namespace resources were used.

## Reproduce in this orb

Prerequisites: the pinned npm dependencies, Docker with Buildx and a default bridge, and Amp's
managed-service CLI. No cloud login is needed. The runner clears the process environment,
uses a fresh HOME without provider credentials, disables Wrangler telemetry, and passes
`--local` explicitly. It does not create a portal or tunnel.

From the repository root:

```sh
npm ci --prefix deploy/cloudflare
bash deploy/cloudflare/test/local/start.sh --ingress-fixture
node deploy/cloudflare/test/local/run.mjs
npm --prefix deploy/cloudflare test
npm --prefix deploy/cloudflare run check
```

The start script invokes this command for each of `inference`, `admin`, and `control`,
under `amp orb service start`. Paths and isolated HOME are expanded by the script:

```sh
env -i PATH="$PATH" HOME="$ISOLATED_HOME" WRANGLER_SEND_METRICS=false \
  DOCKER_HOST=unix:///var/run/docker.sock \
  MINIFLARE_CONTAINER_EGRESS_IMAGE=127.0.0.1:5000/bifrost-local-ingress:fixture \
  node deploy/cloudflare/node_modules/wrangler/bin/wrangler.js dev --local \
  --config deploy/cloudflare/.wrangler/local-e2e/inference.json \
  --persist-to /tmp/bifrost-wrangler-inference-state \
  --show-interactive-dev-session=false
```

The last command illustrates the inference invocation; use the script for all three exact
commands and startup readiness checks. Never substitute `--remote` or `deploy`.

For the existing disposable PostgreSQL fixture and mock-only facade checks:

```sh
docker start bifrost-test-postgres
DEPLOY_TEST_POSTGRES=1 go test ./deploy/cloudflare/container -count=1
CODEX_TEST_POSTGRES=1 go test -race ./framework/codex ./integrations/headroom -count=1
/tmp/headroom-venv/bin/python -m unittest discover -s deploy/modal/headroom -p 'test_*.py'
```

On a fresh orb, create the disposable database with `postgres:17.6-alpine`, host networking,
`POSTGRES_HOST_AUTH_METHOD=trust`, and `POSTGRES_DB=deployment_test`. Never point these
tests at a real database. `.agents/setup` provisions a separate test Python environment at
`/tmp/bifrost-headroom-test`; its `bin/python` can replace the existing venv path above.

## Regression discovered by the HTTP run

Before the fix, aborting the HTTP client left the Container stream running and its lease
present beyond the test's 15-second deadline. In-memory stream unit tests did not detect it.
Adding `enable_request_signal` to both production Worker configurations made the same
HTTP disconnect test pass. The test checks both lease removal and upstream socket closure.
A config regression test prevents removing the flag or shipping a test alias.

A second regression exercised the production handler with a closed readiness port. The
SDK's retry limits could leave startup pending while waiting on its monitor. The handler
now imposes a separate 55-second wall-clock deadline and races cancellation against the SDK.
On timeout it returns a generic 503 and releases the lease, without replaying inference.
The test subclass reduces that deadline to one second; the same test timed out at five
seconds before the fix. It also verifies zero leases after failure. The underlying instance
can remain until normal idle cleanup; the handler does not kill another request's container.

The fixture also needed two corrections: SSE events can span network chunks, and refused
CONNECT targets must return HTTP 400 instead of resetting the runtime connection.
These were harness defects, not production changes. Unexpected failures still fail the run.

## Results and boundaries

The HTTP suite passed all seven groups: rejected requests without wake; authenticated wake and
header stripping; replay/rates; Access claims and the private admin binding; SSE/cancellation
and active-stream idle protection; stop/restart with persistent admission state; and bounded
readiness failure. It also tests chunked oversize bodies and all three inference routes.
The idle-expiry hook is invoked by the test controller; it does not wait five wall-clock minutes.

The Node suite additionally checks IP aggregation, daily limits, and local SQLite DO state
across Miniflare restart. The Modal mock suite checks authentication, routes, chunked limits,
timeout/corruption rejection, concurrency saturation, and slot recovery. The optional official
compressor test is skipped in mock-only mode. PostgreSQL tests use actual local PostgreSQL,
not an implementation of Neon pooling, TLS, autosuspend, replication, or recovery.

Live-only gates remain:

- Container regional placement, image pull/cold-start distributions, resource limits,
  production scheduling and `max_instances` enforcement (ignored by local development).
- Real Cloudflare Access/tsidp login, policy enforcement, device posture, session revocation,
  JWKS rotation/fetch failures, and alternate-hostname protection.
- Durable Object global routing, eviction, storage replication, distributed alarms, quotas,
  and production failure recovery. Local SQLite persistence is not proof of these properties.
- Production networking, DNS, TLS, egress policies and cancellation through Cloudflare's edge.
- Modal proxy authentication, platform scaling and CPU/GPU costs; Neon TLS/pooler/suspension
  and restore behavior; provider OAuth and inference. All billing remains unmeasured.

## Cleanup

```sh
amp orb service stop wrangler-control
amp orb service stop wrangler-admin
amp orb service stop wrangler-inference
docker stop bifrost-local-registry
```

The HTTP suite stops its application container in `finally`. After services stop, remove
generated `.wrangler/local-e2e` files and the recorded isolated HOME when no longer needed.
Do not delete the preserved source bundles or unrelated services. Local Docker images and
the disposable database can remain for repeat tests; deleting them requires no cloud cleanup.

References: https://developers.cloudflare.com/containers/local-dev/,
https://developers.cloudflare.com/workers/configuration/compatibility-flags/,
https://github.com/cloudflare/proxy-everything,
https://github.com/cloudflare/workers-sdk/issues/14056.
