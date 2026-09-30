# Stage 1 deployment and validation

## Approval comes before live work

**Current state: no deployment authorization or account configuration.** No live endpoint exists. All entries in the live matrix remain pending.

One safe next action: reply in the Puck thread with **“Approve Stage 1 only: one basic Container and signed lifecycle-only wake route for a one-hour test.”** This authorizes neither Stage 2 nor a database/AWS purchase. Account access must then use masked project secrets or the user-operated CLI. Never paste tokens, auth keys, connection strings, or device codes into chat.

The one-hour test is an operational limit, not a Cloudflare billing cap. Billing alerts do not impose a hard cap. A new Workers Paid subscription adds at least $5/month. Cleanup must stop the experiment and remove its route.

## Required account configuration

| System | Configuration |
|---|---|
| Cloudflare | Selected account, Workers Paid/Containers access, permission to deploy this Worker and container, one instance maximum. |
| Cloudflare ingress | No routes initially. Optional dedicated custom hostname for `/wake` and `/status` only after approval. No inference hostname. |
| Cloudflare secrets | `TS_AUTHKEY` and `WAKE_HMAC_KEY` as Worker Secrets, never Wrangler vars or build arguments. |
| Cloudflare logging | Observability disabled by default. No request-header/body capture, Logpush, or verbose enrollment traces. |
| Cloudflare edge protection | Rate-limit the optional hostname at the edge. Keep the DO rate limits regardless of WAF availability. |
| Tailscale enrollment | Reusable, ephemeral, preauthorized key with short expiry and only `tag:cf-proof`. No OAuth admin scope. |
| Tailscale policy | Explicit tag owner, only test operator access to `tag:cf-proof` TCP 443. Remove broad conflicting allow rules. |
| Tailscale DNS | MagicDNS enabled, HTTPS certificates enabled, neutral machine name, Funnel not granted. |
| Test client | A separate authorized Tailnet node and a non-Tailnet network for negative tests. Synchronized clocks. |
| Neon | Nothing for Stage 1. Stage 2 requires eligible private networking, AWS VPC endpoints, public blocking, and a private connector. |

Illustrative Tailnet policy fragment, **not an additive security fix**:

```json
{
  "groups": { "group:proof-operators": ["YOUR-OPERATOR-IDENTITY"] },
  "tagOwners": { "tag:cf-proof": ["autogroup:admin"] },
  "grants": [{
    "src": ["group:proof-operators"],
    "dst": ["tag:cf-proof"],
    "ip": ["tcp:443"]
  }]
}
```

Existing allow-all ACLs override the intended restriction through additive permissions. The container needs no access to other Tailnet nodes for this test.

## User-operated deployment after approval

Run these commands in `prototype/` on a trusted machine. Wrangler uses browser login, not a token pasted into chat:

```sh
npm ci --ignore-scripts
npx wrangler login
npx wrangler whoami
npm test
npm run check
WRANGLER_SEND_METRICS=false npm run dry-run
```

1. Verify the account shown by `whoami`.
2. Deploy the route-disabled configuration with `npx wrangler deploy`.
3. Store the ephemeral Tailnet key with `npx wrangler secret put TS_AUTHKEY`.
4. Enter the key only at the hidden CLI prompt.
5. Create the HMAC key in a private local file:

```sh
umask 077
mkdir -p "$HOME/.config/tailnet-proof"
openssl rand -hex 32 > "$HOME/.config/tailnet-proof/wake.key"
npx wrangler secret put WAKE_HMAC_KEY < "$HOME/.config/tailnet-proof/wake.key"
```

6. After approval covers the wake route, add one dedicated custom domain to `wrangler.jsonc`:

```json
"routes": [{ "pattern": "YOUR-DEDICATED-WAKE-HOST", "custom_domain": true }]
```

7. Keep `workers_dev=false` and `preview_urls=false`.
8. Deploy the configuration with `npx wrangler deploy`.
9. Audit the account for other routes, service bindings, and preview domains.

The Worker rejects all paths except signed empty POST requests to `/wake` and `/status`. It ignores port-switch headers. It returns lifecycle status only. No public route reaches an application port.

To start the test, run:

```sh
node scripts/control.mjs https://YOUR-WAKE-HOST wake "$HOME/.config/tailnet-proof/wake.key"
node scripts/control.mjs https://YOUR-WAKE-HOST status "$HOME/.config/tailnet-proof/wake.key"
```

`running` does not mean enrolled or HTTPS-ready. Discover the actual node name from the Tailscale admin console. The name can acquire a suffix after a restart.

## Live experiment matrix

Record timestamps in UTC, plus local America/New_York time when useful. Store only synthetic responses and redacted diagnostics. Never archive browser authentication flows.

| Experiment | Method | Acceptance/evidence | Current result |
|---|---|---|---|
| Cold enrollment | Wake once, poll private `/healthz` from another Tailnet node. | Start-to-first-HTTPS latency, boot ID, node ID/FQDN, certificate validity. | Pending |
| Public denial | Run negative probes from outside the Tailnet against every deployed hostname. | No health/SSE/inference/admin/Headroom/metrics response. | Pending |
| Private DNS/TLS | Resolve actual MagicDNS FQDN, request HTTPS without `-k`. | Correct private address, valid hostname and chain. Record name changes. | Pending |
| Path and latency | Run `tailscale ping --c 10 ACTUAL-FQDN` and `tailscale netcheck` on the test client. | Direct versus DERP, relay region, RTT distribution. Client netcheck alone does not describe container egress. | Pending |
| Container network | Inspect `/dev/net/tun`, capability mask, and `tailscale netcheck` through approved Cloudflare exec/SSH. | Actual capability set and UDP status, no secret/environment dump. | Pending |
| SSE without lease | Wake once, request `/events` for over 180 seconds. Do not poll lifecycle status during the run. | Expected cutoff near idle expiry. Record last sequence and elapsed time. | Pending |
| Tailnet cannot wake | After confirmed sleep, attempt private HTTPS for 60 seconds without a wake request. | No new node or successful response. | Pending |
| SSE with lease | Start a 10-minute stream. Send signed `/wake` every 45 seconds from a second terminal. | 600 ordered events, one boot ID, no gaps. | Pending |
| Idle after lease | Stop the lease. Wait at least 150 seconds before one status read. | Container stopped and private service unavailable. | Pending |
| Restart cleanup | Repeat five sleep/wake cycles. Include one approved abrupt stop. | Record new node IDs, FQDNs, certs, and cleanup after 60 minutes. | Pending |
| Replay/rate control | Repeat an identical signed request concurrently. Then exceed six new wakes in a minute. | One acceptance, replay 409, excess wake 429. | Pending |
| Cold-start recovery | Wake after clean sleep and after abrupt stop. | Enrollment and private service recover within the agreed threshold. | Pending |
| Billing | Compare start/stop timestamps with Cloudflare usage counters. | Measured active seconds, vCPU-seconds, egress, and extrapolated monthly cost. | Pending |

During the idle experiment, lifecycle polling can disturb the measurement if the SDK recreates a DO and resets its in-memory timer. Observe from the private client first. Read lifecycle status once after the expected stop.

Private probe examples (run only from the authorized Tailnet node):

```sh
curl --fail --max-time 30 https://ACTUAL-FQDN/healthz
curl --fail --no-buffer --max-time 660 https://ACTUAL-FQDN/events
tailscale ping --c 10 ACTUAL-FQDN
```

The unleased stream is expected to fail. A successful 10-minute leased stream does not prove indefinite availability or immunity to host eviction.

Public negative examples (run without Tailnet access):

```sh
curl -sS -o /dev/null -w '%{http_code}\n' https://YOUR-WAKE-HOST/healthz
curl -sS -o /dev/null -w '%{http_code}\n' https://YOUR-WAKE-HOST/events
curl -sS -o /dev/null -w '%{http_code}\n' https://YOUR-WAKE-HOST/admin
curl -sS -o /dev/null -w '%{http_code}\n' https://YOUR-WAKE-HOST/metrics
curl -sS -o /dev/null -w '%{http_code}\n' -X POST https://YOUR-WAKE-HOST/v1/responses
curl -sS -o /dev/null -w '%{http_code}\n' -X POST https://YOUR-WAKE-HOST/wake
```

All wake-host probes must return 404 without authentication. Also probe `/v1/chat/completions`, `/headroom`, `/dashboard`, query-string targets, and port-switch headers. Probe the actual Tailnet FQDN off-Tailnet with a bounded timeout. DNS resolution alone does not prove exposure or privacy. Audit Cloudflare route configuration and Tailscale Funnel configuration alongside network tests.

**Stage 1 passes only after live enrollment, private reachability, public denial, leased SSE, wake recovery, and repeatable idle shutdown succeed.** Record latency thresholds before the live run. Suggested experiment thresholds are 90 seconds to private readiness and no missing leased SSE events. A name change is not an automatic failure, but Stage 2 requires an approved discovery/stable-name strategy.

## Health, logging, upgrades, rollback, and cleanup

The supervisor emits fixed lifecycle labels only. `tailnet_ip_ready` means assigned Tailnet IP, not valid Serve TLS. The Docker health check also checks local HTTP. Cloudflare does not necessarily enforce Docker `HEALTHCHECK`, so the supervisor enforces its own 90-second Tailnet health deadline.

The SDK still logs lifecycle events internally. Observability remains disabled by default. Before enabling it, review the SDK logs for sensitive fields. Never use verbose authentication logs or log request bodies. Never run `env`, unrestricted `docker inspect`, or process-environment dumps with live secrets.

For upgrades:

1. Resolve new official image digests and update the lockfile deliberately.
2. Run every offline test and the full dry run.
3. After approval, repeat the live matrix with synthetic data.
4. Record the Worker version, image digest, and result timestamps.

For rollback:

1. Disable the wake route if the private boundary is uncertain.
2. Keep the last validated Worker version and container image available.
3. After approval, redeploy the previous source and image together.
4. Preserve the DO replay ledger and repeat public negative probes.

A Worker-only rollback does not guarantee the matching container rollout. Never delete an image still needed by a rollback target.

For cleanup after the approved experiment:

1. Stop lifecycle leases and wait for the container to stop.
2. Remove the public custom-domain route.
3. Delete the experiment Worker and associated container resources through the Cloudflare dashboard.
4. Remove unused experiment images after rollback retention is no longer required.
5. Revoke the ephemeral auth key and remove stale experiment nodes.
6. Remove the local HMAC key and the experiment Worker secrets.
7. Verify no active instance, route, or experiment resource remains in billing inventory.

## Stage 2 secrets and persistence remain separate

The real stack requires additional approval and a private Neon path. Its future database role must have only the necessary database/schema privileges. Migrations need a separately controlled path. Pooled application connections require verified TLS and tested connection limits.

The Bifrost encryption key must be a separate Cloudflare Secret. Keep its recovery copy in an independent password manager or secret vault. Do not store it in Neon, a database backup, the image, or Git. A database backup without this key cannot recover encrypted credentials. A rollback must preserve the correct key version and database compatibility.

Tailscale node identity, Serve certificate cache, Headroom process memory, local metrics, and unpersisted compression state remain ephemeral. Durable model/account data belongs in the validated persistence layer. CCR, semantic/answer caching, and cross-owner learned-state reuse must stay disabled.
