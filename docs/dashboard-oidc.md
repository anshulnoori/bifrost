# Tailscale dashboard sign-in

This fork supports native tsidp OIDC login. It creates an independent Bifrost admin session after identity verification.
Cloudflare Access can protect the outer connection. It does not replace this session or provide trusted identity headers to Bifrost.
Codex subscription credentials and inference virtual keys remain separate from dashboard authentication.

## Register the Bifrost client

1. Run tsidp on `idp.silverside-mongoose.ts.net` with its native Funnel support.
2. Verify HTTPS discovery at `https://idp.silverside-mongoose.ts.net/.well-known/openid-configuration`.
3. Register a confidential OIDC client for Bifrost in tsidp.
4. Set its exact callback to `https://YOUR-BIFROST-ADMIN-HOST/api/session/oidc/callback`.
5. Store the client ID and secret in the Bifrost service's private environment.
6. Get your numeric Tailscale user ID from a user-owned node:

   ```sh
   tailscale status --json | jq -er '.Self.UserID | tostring'
   ```

The callback is **not** the Cloudflare Access `/cdn-cgi/access/callback` URL. These clients require separate registrations.
Stock tsidp derives its issuer from the node DNS name. A custom-domain reverse proxy is not necessary for this configuration.
The gateway needs HTTPS access to discovery, token, and JWKS endpoints. The browser must connect through your tailnet for tsidp authorization.
An ACL-tagged node does not identify an ordinary user's subject reliably. The allowed subject must match tsidp's `sub` claim.

## Configure Bifrost

Keep dashboard password authentication enabled. Keep its recovery password in your password manager.
Use the existing `BIFROST_ENCRYPTION_KEY` for this database. Never replace that key to enable OIDC.

Put these variables in a private environment file or secret manager:

```sh
BIFROST_OIDC_ISSUER=https://idp.silverside-mongoose.ts.net
BIFROST_OIDC_CLIENT_ID=YOUR-REGISTERED-CLIENT-ID
BIFROST_OIDC_CLIENT_SECRET=YOUR-REGISTERED-CLIENT-SECRET
BIFROST_OIDC_REDIRECT_URL=https://YOUR-BIFROST-ADMIN-HOST/api/session/oidc/callback
BIFROST_OIDC_ALLOWED_SUBJECTS=YOUR-NUMERIC-TAILSCALE-USER-ID
```

Use comma-separated subjects for multiple administrators. Every allowed subject receives the same local administrator permissions as the recovery account.
The allowlist uses exact issuer and subject values, not email addresses or domain suffixes. Stock tsidp does not emit `email_verified`.
Partial configuration prevents startup. With no OIDC variables, Bifrost retains its existing password login.

After client registration, restart Bifrost with this environment. Select **Sign in with Tailscale** on the login page.
If tsidp is unavailable, select **Use recovery password**. Bifrost never asks for the Tailscale password itself.

## Session and replica behavior

The login flow uses authorization code, S256 PKCE, nonce, and a Secure HttpOnly browser cookie.
The database stores hashes of random browser/state values and an AES-GCM encrypted envelope. Pending state expires after five minutes.
Callbacks consume state atomically before token exchange. A replay or competing replica cannot create another session from that state.
The gateway checks ID-token signature, issuer, audience, expiry, nonce, and allowed subject. It never stores the IdP access, refresh, or ID token.
The local session expires at the earlier of ID-token expiry and 12 hours. Logout deletes the local session, not the Tailscale session.
Subsequent sign-in can reuse the active Tailscale identity. The gateway does not silently refresh IdP tokens.

All replicas need the same database, encryption key, OIDC configuration, and allowlist. Sticky sessions are not required.
PostgreSQL is appropriate for distributed replicas. The regression suite tests concurrent callbacks against shared SQLite, not a live multi-node PostgreSQL deployment.
Removing a subject or removing all OIDC variables rejects affected sessions after restart. Removing access only at tsidp does not revoke existing Bifrost sessions immediately.
Restart every replica after an allowlist change. Password recovery sessions remain independent.

## Upgrade and rollback

The additive `add_dashboard_oidc` migration creates `dashboard_oidc_logins` and two nullable identity columns on `sessions`.
Deploy the new binary to every replica before enabling OIDC. Old binaries do not enforce the OIDC subject allowlist.
Before a downgrade, stop all replicas and revoke the OIDC sessions with an approved database operation:

```sql
DELETE FROM sessions WHERE COALESCE(oidc_issuer, '') <> '' OR COALESCE(oidc_subject, '') <> '';
DELETE FROM dashboard_oidc_logins;
```

Remove the OIDC environment variables before starting the old binary. Keep the additive columns and table for a later upgrade.
Take a database backup before migration or manual maintenance. Do not use mixed old/new replicas after enabling OIDC.

## Proxy and logging requirements

Preserve the browser's Origin header for the login POST. Bifrost compares it with the configured callback origin, not forwarded identity headers.
Allow GET callbacks and retain cookies through the admin proxy. Do not cache login, callback, or session responses.
Redact callback query strings from proxy/CDN logs. Bifrost's HTTP access log omits the callback query, which contains a temporary authorization code.
Never enable request-body/header tracing for credentials. Keep the origin private when Cloudflare Access protects the public admin host.

## Reproducible verification

The signed TLS IdP fixtures require no external account or subscription usage:

```sh
GOMAXPROCS=3 go test -race ./transports/bifrost-http/handlers -run TestDashboardOIDC -count=1
```

UI tests exercise login, recovery, cancellation, and disabled-provider states against mocked auth status:

```sh
cd tests/e2e
BASE_URL=http://127.0.0.1:8080 npx playwright test --config playwright.oidc.config.ts
```

Live acceptance still requires tsidp registration and a browser on the tailnet. Verify login, a protected dashboard request, logout, and a denied subject.
No production client secret belongs in chat, source control, screenshots, or a Git bundle.
