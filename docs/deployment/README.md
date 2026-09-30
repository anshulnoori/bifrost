# Deployment status and architecture

**Current target: [NixOS + Funnel + Neon + Valkey](nixos.md).**
The owner abandoned Cloudflare Workers/Containers. The material below records the previous candidate, not current deployment instructions.
Its code and tests remain for history and reuse. No Cloudflare deployment is required by the NixOS profile.

This implementation is an **offline-tested deployment candidate**, not a live service.
Do not attach public DNS until the release gates in [validation.md](validation.md) pass.
No provider resources were created. No credentials were requested or committed.

```text
Inference client ── scoped VK ──▶ Inference Worker ──▶ single Container
                                 exact allowlist       Bifrost + native bridge
Human ── tailnet/tsidp ──▶ Access ──▶ Admin Worker ──▶ private service binding ──┘
                                                        │
                          ┌─────────────────────────────┤
                          ▼                             ▼
                    Neon PostgreSQL               Modal proxy auth
                    pooled verified TLS           + bridge bearer secret
                    encrypted credentials         official Headroom CPU
```

The Workers share one process through a private service binding. There is no separate
Bifrost admin listener. A compromised inference Worker can therefore cross the internal
trust boundary. Worker IAM and review are part of the security boundary.

Public routes are POST `/v1/chat/completions`, `/v1/responses`, and `/v1/messages`.
The last route maps to Bifrost's `/anthropic/v1/messages`. All other paths fail before
container access. WebSockets, background Responses, result retrieval, model listing,
passthrough, query strings, and compressed HTTP request bodies are deliberately unsupported.
SSE response bytes pass through without parsing. A stream has a ten-minute ceiling.
Requests are never automatically replayed. An optional idempotency key rejects repeats
for 24 hours; it does not return a cached inference result.

Administration requires a verified Access JWT and Bifrost's independent session.
Only the routes in `deploy/cloudflare/shared/policy.mjs` can pass. The restricted dashboard
is not yet browser-validated; adding an API requires a specific route and negative tests.
Neither Worker routes Headroom, metrics, database, plugins, or lifecycle controls.

## Source preservation

The imported combined source is `af0011493589707a851d820616f962109d155b4d`, including
`78af9e3d8df365c7b45e31d2a5ccc37ab5be4b55`. The source bundle SHA-256 is
`6c5452fa4a07600aecb535802b42383b1689f6150dcd3ecf51a9e39e1dbc79b1`.
The prototype tip `6054b19014ad56467f8bcb2cce54a92b6ab7f2a9` remains on
`preserved/cloudflare-stage1` and in merged history. Its tests and directory remain unchanged.
These were source-thread local commits, not an assumed `origin/main` baseline.

Fork-added messages were audited. Inherited upstream history contains attribution trailers.
Do not push this preserved history under a policy that forbids those messages. Obtain an
owner decision about an attribution-clean snapshot or approved history transfer first.
Use `/usr/bin/git` locally: the installed Git wrapper injects trailers.

## Operating policy

- One Container instance; five-minute idle stop, with persisted expiring stream leases.
- Invalid credentials, route, body, or model fail before container wake.
- Four-MiB inference bodies; one-MiB admin bodies; no raw body logging.
- Edge registry expiry, model restrictions, per-key request rate/daily caps, per-IP cap.
- Configure independent Bifrost VK provider/model/token budgets. Edge request counts are
  not dollar budgets. Use one key per client and never an admin password for inference.
- Provider prefix caching is unchanged. Do not enable semantic/response cache plugins.
- Headroom is disabled in the production launcher until latency and quality gates pass.
  The official CPU implementation is available; no GPU benefit is claimed.
- Compression uses isolated subprocesses. CCR, caches, learned memory, ML downloads,
  telemetry, beacon, and marker/redrive behavior are disabled.
- Compression failures bypass unchanged input under the default open policy. Applications
  requiring mandatory compression must explicitly use the existing closed policy.
  Do not compress signed reasoning, unsupported multimodal input, or raw streams.

See [setup.md](setup.md), [operations.md](operations.md), [security.md](security.md),
[validation.md](validation.md), and [costs.md](costs.md).
