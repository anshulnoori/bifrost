# Operations and recovery

## Deploy, upgrade, and roll back

Keep a release record with source revision, image digest, Worker versions, Modal version,
migration version, configuration checksum, and backup timestamp. Never include secret values.
Build UI, binary, and native plugin together. Never replace only the `.so`.
Base images are digest-pinned; Alpine package resolution and the Modal base image remain
mutable dependencies. Capture an SBOM and pin those before claiming bit-reproducible builds.

For upgrades, disable inference, wait up to ten minutes for streams, take a recoverable
database snapshot, migrate once with the direct role, and stage the matching image.
Apply the runtime grants again for new tables. Run the matrix in `validation.md` before
reenabling inference. A process restart does not rotate the encryption key.

For rollback, disable inference and drain first. Roll back the two Worker versions and the
Container/Modal release as a compatible set. Do not assume a schema downgrade is safe.
If the old application cannot read the new schema, restore a new Neon branch from the
pre-upgrade recovery point and validate it before switching the pooled URL secret.
Account for writes lost after that recovery point. Never overwrite the original branch
until recovery is accepted. Test credentials with the original encryption key.

## Keys and sessions

For routine rotation, issue a new per-client Bifrost VK, give it the same or tighter budgets,
and add its digest to the edge registry. Move that client, then remove the old digest and
revoke the old VK in Bifrost. Check both direct revocation and edge rejection.
Registry updates do not cancel already admitted streams. For emergency containment, set
`EMERGENCY_DISABLE=true`, revoke the VK, and stop the Container if active work must end.
The IP cap is supplementary: NATs can share an address and attackers can change addresses.

For a stolen admin session, revoke Access sessions and the upstream IdP session, then
rotate the Bifrost password and invalidate Bifrost sessions. Review account changes,
VK creation, provider changes, and deployment changes. Do not rely on the Access cookie
alone: the Worker independently checks signature, issuer, audience, time, email, and subject.

For suspected Worker compromise, disable routes, rotate database and Modal credentials,
and revoke OAuth/provider tokens. For Container compromise, also assume the encryption key
was read. Rotate protected account credentials and re-encrypt through an audited migration;
merely changing the encryption-key secret can destroy access to existing records.

## Database outage, backup, and restore

Each config/log pool is capped at five active and one idle connection. Two stores and
temporary migration pools consume more than five connections in total. Account for this
when selecting a Neon plan. Runtime lifetime is five minutes and idle lifetime is 30 seconds.
The runtime role has 30-second statement, five-second lock, and 30-second idle-transaction
timeouts. Do not retry inference automatically after an ambiguous provider response.
Use existing Codex CAS refresh fencing; do not wrap token refresh in blind retries.

During database failure, startup must fail closed and existing requests can fail. Do not
switch to SQLite or store refresh tokens in scratch. Restore connectivity and verify VK
revocation plus refresh fencing before admitting clients again. Neon suspension/wake and
pooler behavior still need live tests; local PostgreSQL is not a substitute.

Confirm the selected plan's restore window in Neon's console. Retention is not a backup
unless an actual restore succeeds. Keep encrypted logical backups in owner-controlled
storage if required by the recovery objective. Never put dumps in this repository or chat.
Quarterly, restore a separate isolated branch, inject a recovery copy of the encryption key,
verify account emails, encrypted tokens, reserves, owner isolation, and VK permissions,
then destroy the test branch after approval. Do not make paid provider calls during restore
validation. Record recovery time and the oldest recoverable timestamp.

## Observability without bodies

The launcher discards upstream process output because upstream error strings have not
been completely audited for credentials. It emits only fixed lifecycle events. This is
a privacy-first limitation, not full operational observability. Content logging and request
overrides are disabled; seven-day Bifrost metadata storage is configured in PostgreSQL.
The Headroom in-memory metrics endpoint remains loopback-only and is not durable.

Before production, configure private provider-native dashboards and alerts for:

| Signal | Initial alert | Dimensions allowed |
| --- | --- | --- |
| Worker errors | 5xx above 5% for five minutes | plane, status class, release |
| Cold start | readiness timeout or p95 above 45 seconds | region, release |
| Streams | duration ceiling, interrupted-stream rise | route class, release |
| Modal | timeout above 1%, queued work, max containers | policy version, CPU/GPU type |
| PostgreSQL | pool saturation, query timeout, unavailable | store, operation class |
| OAuth | refresh conflict/error rate, disconnected account | opaque account ID only |
| Abuse | unexpected request-rate rise or budget exhaustion | rotated key hash, no email |
| Spend | 50%, 80%, 100% of owner budget | provider, day |

These are commissioning requirements; remote dashboards and alert rules were not created.
Never export prompts, responses, authorization headers, emails, device codes, or database
URLs. Keep actual provider token usage separate from Headroom estimates. Missing usage
is unknown, not zero. Compression estimates are not billed savings.

## Cleanup

Disable inference and drain first. Remove custom-domain routes only after deciding whether
to return maintenance responses. Delete Workers, Container instances, and DO state only
after approving loss of admission history. Stop the Modal app in its dashboard, revoke proxy
tokens, and remove unused secrets. Delete Neon branches/projects only after accepting data
loss and testing required recovery copies. Revoke tsidp's OIDC client and remove Access
policies when the admin service is retired. Do not delete a shared tailnet identity service.
