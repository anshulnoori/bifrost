# Threat model and drills

```text
┌──────────────────── Untrusted Internet ─────────────────────┐
│ inference credentials                  browser / spoofed JWT │
└─────────────┬───────────────────────────────────┬─────────────┘
              ▼                                   ▼
┌──── Edge VK policy ──────┐             ┌── Access + owner JWT ─┐
│ route/body/model/rate   │             │ Bifrost session       │
└─────────────┬───────────┘             └──────────┬────────────┘
              └────────────────┬──────────────────┘
                     ┌─────────▼───────────┐
                     │ trusted process     │
                     │ provider authority  │
                     └────┬─────────┬───────┘
                   TLS    ▼         ▼   two service credentials
                       Neon       Modal
```

| Threat | Control | Drill and residual risk |
| --- | --- | --- |
| Leaked inference VK | Hash registry, expiry, model list, request caps, Bifrost scoped budgets | Revoke at both layers. Already admitted streams continue; dollar budgets depend on provider usage reporting. |
| Spoofed Access headers | RS256 JWT verification twice, exact issuer/audience/owner, header allowlist | Send fabricated and expired JWTs plus spoofed email headers; no wake. JWKS availability can deny legitimate access. |
| Stolen owner session | Short Access lifetime, upstream MFA, device policy, Bifrost auth | Revoke both sessions. A thief holding both sessions can use allowed admin APIs. |
| Compromised Worker | Separate public entrypoints, private service binding, deployment IAM | Revoke deployment access and secrets. Inference Worker possesses Container secrets; this is not hardware isolation. |
| Compromised Container | Non-root, Tini, no Python sidecar, minimum credentials | Rotate all readable secrets and OAuth credentials. Outbound Internet is required; an attacker can exfiltrate. No egress firewall is implemented. |
| Compromised Modal | Proxy auth plus independent bridge token, isolated subprocess, no provider credentials | Revoke proxy/bridge credentials. Service can read submitted tool text; avoid sensitive compression if this trust is unacceptable. |
| Neon credential theft | TLS verification, runtime DML-only role, encrypted Codex secrets | Rotate role, inspect changes. Metadata and ciphertext remain exposed; a stolen encryption key defeats encryption. |
| Encryption-key loss | Separate offline recovery vault | Restore a test branch with the vault copy. No key recovery backdoor exists. |
| Cache poisoning | Semantic/response/CCR caches off, per-request isolation | Cross-tenant fixtures; provider prefix caching remains provider-controlled. |
| Replay | Transactional 24-hour idempotency-key rejection, no automatic retries | Race 32 identical requests; exactly one admission. Requests without a key are bounded by rates, not deduplicated. |
| SSRF/routing override | Exact public paths, no passthrough, stripped headers, fixed Modal host | Fuzz paths and routing fields. Owner-authorized provider configuration still needs review. |
| Supply chain | Locked dependencies, pinned base digests, same Go build graph | Rebuild and inspect SBOM before upgrades. Some OS packages and Modal base remain mutable. |
| Disk persistence | No production SQLite, scratch-only generated config with env references | Start without Neon and verify failure. Cloudflare root filesystem read-only enforcement is not configured by this SDK. |
| Prompt logging | Content logging off, stripped overrides, discarded process output | Search synthetic canaries in exported logs. Provider-side platform logging controls still require live verification. |

Keep all drill fixtures synthetic. Do not photograph device flows or authentication pages
containing codes. Test public route scanning against both hostnames and any deployment
aliases; an unprotected alternate hostname defeats the architecture.
