# Validation record and release gates

No Cloudflare, Modal, Neon, Tailscale, or paid Codex endpoint was live-validated in this work.
No public deployment endpoint or portal exists. Local fixtures contain synthetic credentials.

## Executed checks

| Check | Result |
| --- | --- |
| Bundle per-part/original SHA-256, self-contained import, exact source ancestry | Passed |
| `npm test` in `deploy/cloudflare` | 12 passed, including real DO restart and 32-way replay race |
| `node deploy/cloudflare/test/local/run.mjs` against three Wrangler dev servers | 7/7 HTTP groups passed; see transport limitations below |
| `npm run check` and `npm run dry-run` in `deploy/cloudflare` | Both Workers passed |
| `npm test` and `npm run check` in `prototype` | 11 passed; preserved Stage 1 baseline |
| `CODEX_TEST_POSTGRES=1 go test -race ./framework/codex ./core/providers/codex ./integrations/headroom -count=1` | Passed |
| Targeted transport handler/lib Codex and Headroom tests | Passed |
| `DEPLOY_TEST_POSTGRES=1 go test ./deploy/cloudflare/container -count=1` | Passed; repeat migration and restricted runtime schema startup |
| Modal facade unit tests | 5 mock tests passed; optional official compressor skipped in the latest mock-only run (passed previously) |
| Real compiled gateway Codex fixture | Passed onboarding, refresh, unary Responses, Chat SSE, isolation, disconnect |
| Real compiled gateway + native Headroom plugin + official compressor | Passed; earlier concurrent-build timeout retained as a performance finding |
| `docker build --network host -f deploy/cloudflare/container/Dockerfile -t bifrost-cloudflare:local .` | Passed, linux/amd64 |
| UI enterprise build and typecheck | Passed; no appearance changes |
| Modal 1.5.5 definition import | Passed locally; no deployment |
| `.agents/setup` twice | Passed; no module synchronization or provider login |

The image builds binary and plugin in one Go 1.27.0 workspace with identical flags.
The local gateway integration exercised native plugin loading. The final musl image has
not completed an authenticated Neon-backed end-to-end startup.

The [Wrangler local run](local-validation.md) exercises the production Worker handlers,
private admin binding, Container SDK, and Docker mock application. This orb lacks the
stock sidecar's required kernel socket match, so the passing run used an explicit
ingress-only transport fixture. It found and fixed incoming cancellation and hard startup
deadline defects. It is not evidence for Cloudflare production networking or placement.

The edge tests verify allowlists, duplicate JSON-key rejection, JWT signature/issuer/audience/
expiry/identity, spoofed headers, size limits, model restriction, rates, replay persistence,
byte-preserving SSE, cancellation, and dashboard bootstrap route policy. They do not prove
the deployed SDK's Container start/stop behavior. Full browser login through both auth
layers, sustained multi-agent load, and cloud chaos testing remain outstanding.

## Staged live matrix

Keep inference disabled until each applicable gate passes. Use synthetic prompts first.

1. Neon: run the controlled migration, apply runtime grants, verify actual TLS hostname
   validation and pooled connections, then start twice without DDL privileges. Restart after
   persisting encrypted accounts and verify emails/reserves without displaying tokens.
2. Access: verify discovered tsidp endpoints, allowed owner login, wrong identity denial,
   expired token denial, fabricated header denial, and continued Bifrost password protection.
   Check provider/Codex and VK pages in a browser. Never capture OAuth/device codes.
3. Cloudflare: cold-start one authenticated synthetic inference request, record readiness
   time, stream SSE, cancel midstream, hold a second stream over idle expiry, and confirm
   stop occurs only after drain. Repeat after a Worker/DO restart.
4. Scan both hostnames and every alternate alias for `/health`, `/metrics`, `/api/plugins`,
   `/api/headroom/events`, `/v1/compress`, `/debug/pprof`, `/wake`, unknown paths, encoded
   paths, and non-allowlisted methods. Inference must not route these even with a valid VK.
5. Modal: anonymous and single-credential requests must fail. Confirm size limits, no
   request body logs, timeout/bypass, official version, concurrency cap, and idle teardown.
   Run CPU/GPU comparisons only after approving the budget. Leave GPU disabled otherwise.
6. Load/chaos: run concurrent synthetic clients, refresh races, key revocation during streams,
   Container termination, Modal timeout, Neon suspend/wake/outage, and ambiguous upstream
   disconnect. Assert no replay, cross-owner tokens, SQLite fallback, or structured-field
   corruption. Record failures rather than treating successful HTTP status alone as proof.
7. Restore a separate Neon branch and rehearse rollback before real accounts are admitted.
   Verify alert delivery and privacy using a synthetic log canary.
8. Run authorized live Codex Chat/Responses unary/SSE through the full path. Enable
   compression only after the latency/fidelity policy is accepted and the launcher profile
   is deliberately updated and rebuilt. Never call estimated reduction billed savings.

## Remaining implementation limitations

- Headroom compression and GPU routing are disabled, not measured production optimizations.
- Durable Headroom metrics export, provider dashboards, and alerts are commissioning work.
- Exact OS package/Modal base pins and SBOM review remain supply-chain work.
- There is one shared trusted Bifrost process, unrestricted required outbound Internet,
  and no SDK-enforced read-only root filesystem setting.
- Startup checks a TCP port; the image has an HTTP health probe, but cloud readiness and
  liveness behavior still need validation. Long streams have a deliberate ten-minute limit.
- Upstream automatic schema/index checks still run. Runtime DML permissions prevent schema
  mutation; the distinct migration procedure remains mandatory.

These limitations mean the full requested production acceptance criteria are **not yet met**.
