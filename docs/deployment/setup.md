# Owner-operated provisioning

These steps change external state and can incur costs. They are instructions, not actions
performed by the implementation thread. Use masked provider dialogs. Never paste a
credential, device code, login link, or connection string into chat or a terminal command.
Disable shell tracing and terminal recording during credential operations.

## 1. Neon

Create a dedicated project, database, migration role `bifrost_migrator`, and restricted
runtime role `bifrost_runtime` in Neon's dashboard. Select a region near the Container.
Use generated passwords. The runtime URL must use the `-pooler` hostname and
`sslmode=verify-full`; the migration URL must use the direct hostname.
The launcher accepts only `postgresql://` Neon hosts and verified TLS. It does not use
SQLite when PostgreSQL is missing or unavailable. A network-public Neon host is not an
anonymous database; TLS and database authentication remain required.

Build the image before provisioning:

```sh
docker build --platform linux/amd64 -f deploy/cloudflare/container/Dockerfile -t bifrost-cloudflare:release .
```

Run migration once during a maintenance window, with inference disabled. Inject the
direct migration URL from a masked prompt, then remove the variable immediately:

```sh
read -rsp 'Direct migration URL: ' NEON_DATABASE_URL; printf '\n'
export NEON_DATABASE_URL
docker run --rm --read-only --tmpfs /tmp:rw,noexec,nosuid,size=128m \
  -e NEON_DATABASE_URL bifrost-cloudflare:release --migrate
unset NEON_DATABASE_URL
```

The migration process must be the only schema writer. The launcher does not coordinate
multiple migration jobs. Apply `deploy/neon/runtime-role.sql` using the migration role
after migration. Do not use a superuser or database owner as the runtime role.
Runtime startup still invokes upstream schema checks: **test startup with the restricted
role before release**. If it requires DDL, change the startup path; do not grant DDL to fix it.
The local migration test passes with a DML-only runtime role after migration. Logstore's
background index builders can still attempt DDL and log nonfatal failures. Confirm this
behavior and readiness against Neon before release; do not grant runtime DDL privileges.

## 2. tsidp and Cloudflare Access

Run tsidp on an owner-controlled persistent host. Pin an audited release or image digest;
do not use a mutable `latest` tag. Persist its `-dir`/`TS_STATE_DIR`. Loss of that state
loses client registrations and sessions. Cloudflare's SaaS backchannel requires tsidp's
`--funnel` mode. Limit tsidp admin/DCR application capabilities to the owner's identity;
do not copy the upstream wildcard example. Do not enable STS or DCR unless needed.

Read the deployed discovery metadata:

```sh
bash deploy/tailscale/verify-discovery.sh https://idp.YOUR-TAILNET.ts.net
```

In Access, add a generic OIDC provider. Register a confidential client in tsidp with exact
callback `https://YOUR-TEAM.cloudflareaccess.com/cdn-cgi/access/callback`. Copy the
authorization, token, and JWKS URLs from discovery, not guessed paths. Transfer the client
secret only between masked provider controls. Request `openid profile email`. Validate
the actual returned email and subject during owner-only staging; do not invent claim values.

Create one self-hosted Access application for the entire admin hostname, with a one-hour
or shorter session. Allow only the owner's exact email through this identity provider.
Require managed-device posture where supported. Enforce phishing-resistant MFA at the
tailnet's upstream identity provider. tsidp's tailnet identity alone is not proof of a fresh
MFA challenge; do not impose an unverified `acr`/`amr` expectation.
There must be no bypass or service-token policy for human administration.

Record Access issuer and application audience in both Worker configurations. Store the
verified Access **subject**, not an assumed tsidp subject, as `OWNER_SUB`. Store
`OWNER_EMAIL` as a secret to avoid publishing the owner's email. Both Workers need these.

### Native Bifrost sign-in

For sign-in without a second password prompt, follow [dashboard OIDC](../dashboard-oidc.md).
The intended issuer is `https://idp.silverside-mongoose.ts.net`; it is not yet live-verified.
Discovery, exact issuer equality, and valid TLS remain deployment gates.
Register a separate confidential Bifrost client with callback
`https://YOUR-BIFROST-ADMIN-HOST/api/session/oidc/callback`, not the Access callback above.
Keep Bifrost auth and the existing encryption key enabled; the password is for recovery.

Store all five `BIFROST_OIDC_*` settings documented there as secrets on the inference Worker,
which owns the Container binding. They are forwarded only to the container environment.
Do not put client secrets, subject allowlists, or callback codes in Wrangler vars or logs.
With no settings, password login remains available; partial configuration fails startup.
The admin Worker permits only the login POST and callback GET, still behind Access.
It preserves the OIDC browser-binding cookie only on those routes and bounds callback query parameters.
Disable callback URL/query logging at every proxy/CDN layer; Worker observability stays off.

Run the controlled migration and apply runtime grants before deploying the new binary to all replicas.
Enable OIDC only after every replica is upgraded. Before rollback, revoke OIDC sessions through
the operator-approved procedure in the OIDC guide; old binaries do not enforce its subject allowlist.

## 3. Modal

The local SDK definition was imported with `modal==1.5.5`. Install that exact version in
an isolated environment. Authenticate using Modal's owner-operated CLI flow without
sharing its output. Create the named secret `bifrost-headroom` in the masked dashboard
with `HEADROOM_PROXY_TOKEN` (at least 32 random characters).

After billing approval, run:

```sh
modal deploy deploy/modal/headroom/app.py
```

Create a dedicated **proxy-auth token** in Modal for this endpoint. It is not the deployment
API token. Store its ID/secret in Cloudflare as `MODAL_TOKEN_ID`/`MODAL_TOKEN_SECRET`.
The endpoint also requires `X-Headroom-Proxy-Token`. Anonymous health/version requests
must fail. No custom domain or public unauthenticated proxy is needed.
CPU2, 2GiB, concurrency1, maximum2 containers, minimum0, and 60-second idle timeout are
specified. Set a billing alert manually; these limits do not constitute a hard spend cap.

## 4. Cloudflare and secrets

Set the two exact HTTPS origins and Access issuer/audience in the Wrangler files.
Keep `EMERGENCY_DISABLE=true`. Configure dedicated custom-domain routes only after the
Access application exists. Keep `workers_dev`, preview URLs, and request logs disabled.
Do not create a public Container port or an alternate hostname that bypasses Workers.

From `deploy/cloudflare`, use Wrangler's interactive secret prompt, once per name:

```sh
npx wrangler secret put NEON_DATABASE_URL --config inference-worker/wrangler.jsonc
```

Inference Worker secrets:
`NEON_DATABASE_URL`, `BIFROST_ENCRYPTION_KEY`, `BIFROST_ADMIN_PASSWORD`,
`HEADROOM_ENDPOINT`, `HEADROOM_PROXY_TOKEN`, `HEADROOM_SCOPE_KEY`,
`HEADROOM_METRICS_TOKEN`, `MODAL_TOKEN_ID`, `MODAL_TOKEN_SECRET`,
`INFERENCE_KEYS_JSON`, `OWNER_EMAIL`, `OWNER_SUB`.
Admin Worker secrets: `OWNER_EMAIL`, `OWNER_SUB`.
Generate independent random values of at least 32 characters where applicable.
The Headroom endpoint is the HTTPS Modal origin without a trailing slash.
Store the encryption key in an offline encrypted recovery vault separate from Neon.
Never regenerate it during restarts. Loss makes encrypted account data unrecoverable.

The inference registry maps each VK's lowercase SHA-256 digest to
`{"expires": UNIX_SECONDS, "models": ["EXACT_MODEL"], "rpm": 20, "daily_requests": 100}`.
Build this JSON in a private password-manager note, not tracked files. The registry contains
no plaintext key. Issue the same key in Bifrost with provider/model restrictions and budgets.
The launcher enforces VK authentication independently. Unknown keys fail at the edge.

After explicit deployment approval:

```sh
npm ci
npm test && npm run check && npm run dry-run
npx wrangler deploy --config inference-worker/wrangler.jsonc
npx wrangler deploy --config admin-worker/wrangler.jsonc
```

Attach the inference and admin custom domains to their respective Workers. Do not reuse
the prototype's signed lifecycle endpoint. Log in through Access, then use Bifrost's Tailscale
sign-in if configured, or the `owner` recovery password. Configure Codex accounts and VKs only in that protected UI.
Complete the staging matrix before setting `EMERGENCY_DISABLE=false`.

## Sources and unresolved provider checks

- https://github.com/tailscale/tsidp — persistent state, capabilities, SaaS Funnel requirement.
- https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/generic-oidc/
- https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/authorization-cookie/validating-json/
- https://modal.com/docs/guide/webhook-proxy-auth
- https://neon.com/docs/connect/connection-pooling

Discovery, actual claim mapping, device posture support, certificate validation against
the selected Neon endpoint, and the complete Access login are live release gates.
