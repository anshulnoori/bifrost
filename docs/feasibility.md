# Feasibility and evidence — 2026-09-23

## Verdict: conditional private networking, not a completed stack

**Cloudflare feasibility remains unproven until the live test.** Local Docker does not reproduce Cloudflare routing, placement, egress, or shutdown behavior.

**A sleeping Container cannot accept Tailnet traffic.** Its `tailscaled` process and DERP connection no longer exist. Cloudflare documents Worker/DO activation, not a Tailnet wake integration [1][2]. This is an architectural inference, not a live measurement.

**Tailnet traffic does not renew `sleepAfter` in SDK 0.3.7.** Its activity accounting surrounds Worker-proxied requests and WebSockets [3]. Private streams bypass that accounting. The prototype tests both unmodified sleep and explicit lifecycle leases.

**The strict Neon requirement blocks the simple Cloudflare + Neon topology.** Standard Neon connections use a public endpoint [9]. Verified TLS protects transport, not network exposure. Neon Private Networking requires AWS PrivateLink and `block_public_connections=true`. Cloudflare alone does not supply that AWS network path.

A possible extension uses a Tailnet TCP connector inside an AWS VPC with a Neon endpoint. This requires separately approved infrastructure and testing. A userspace Tailscale client does not transparently route ordinary Go Postgres sockets. That extension needs an explicit local TCP proxy, correct private DNS, and preserved Neon TLS hostname verification. It is not implemented here.

## Runtime constraints and their evidence

| Topic | Documented behavior or source observation | Prototype consequence |
|---|---|---|
| Architecture | Cloudflare requires `linux/amd64` [1]. | Both base images use amd64 manifest digests. |
| TUN and capabilities | Cloudflare documents unprivileged execution and no iptables manipulation [4]. It does not provide a clear TUN/NET_ADMIN guarantee. | Use userspace mode. Local tests remove all capabilities and provide no TUN. Actual Cloudflare capability set remains unmeasured. |
| Public network ingress | Native end-user TCP/UDP ingress is unavailable. Normal requests pass through Workers [1]. | No container address or public data proxy. |
| Egress | Internet access defaults to enabled. Non-HTTP egress is denied when `enableInternet=false` [5]. | Enable internet without HTTPS interception. Outbound UDP/NAT behavior needs measurement. |
| DERP | Tailscale uses encrypted relays when direct connectivity fails [10]. | TCP 443 is the fallback candidate. No claim that Cloudflare supports a working DERP session yet. |
| Lifecycle | DO start is explicit. Default `sleepAfter` is ten minutes. Hooks control shutdown [2]. | One named DO, 120-second experiment timer, signed wake and status actions. |
| Process model | Each container runs inside a VM. Platform shutdown sends TERM, then KILL after up to fifteen minutes [1]. | Tini reaps children. The supervisor sends TERM and bounds its own shutdown to ten seconds. |
| Health | SDK port readiness differs from application readiness [2]. | No application port is given to the Worker. Supervisor checks Tailnet IP health. Tailnet client verifies HTTPS readiness. |
| Startup probe | SDK `start()` still probes fallback port 33 [3]. A not-listening error can indicate successful process startup. | Worker lifecycle status means process state only. This SDK behavior must work live. |
| Ephemeral disk | Sleep discards disk and restarts from the image [4]. | Tailscale identity stays in memory. No local persistence claims. |
| Workload identity | Tailscale accepts custom OIDC issuers. Automatic discovery covers AWS, GCP, and GitHub [7]. | No documented Cloudflare runtime OIDC issuer was found. Do not invent federation. Use a limited auth key for this experiment. |

The current Cloudflare docs do not decisively establish outbound UDP hole punching or the exact Linux capability set. The test must record both. Lack of native inbound UDP does not prove that outbound UDP replies cannot work.

## Architecture and trust boundaries

```diagram
Default: no public Worker route, no automatic start trigger

Optional control plane after approval
┌──────────────────┐   HTTPS, signed empty POST   ┌────────────────────┐
│ Operator/client  │────────────────────────────▶│ Lifecycle Worker   │
│ Private key file │                             │ /wake and /status  │
└──────────────────┘                             └──────────┬─────────┘
                                                         │ typed RPC only
                                              ┌──────────▼─────────┐
                                              │ Singleton DO       │
                                              │ Replay/rate ledger │
                                              │ Start / lease      │
                                              └──────────┬─────────┘
                                                         │ lifecycle only
Private data plane                                       ▼
┌──────────────────┐   WireGuard direct or DERP   ┌────────────────────┐
│ Authorized node  │◀───────────────────────────▶│ Cloudflare VM      │
│ Tailnet grants   │                             │ userspace tailscale│
└──────────────────┘                             │ Serve HTTPS :443   │
                                                 │        │           │
                                                 │ 127.0.0.1:8080     │
                                                 │ synthetic HTTP/SSE │
                                                 └────────────────────┘

Stage 2 candidate, NOT IMPLEMENTED
Tailnet → separate inference/admin Serve ports → Bifrost application auth
Bifrost → loopback Headroom and loopback metrics
Bifrost → local userspace TCP proxy → Tailnet AWS connector
        → VPC PrivateLink → Neon (public connections blocked, verified TLS)
```

An existing trusted Worker binding or scheduler can control the DO without public ingress. No such controller exists in this project. A self-contained on-demand test needs the optional wake route. Cron polling can remove the public route, but periodically wakes the container without demand.

## Enrollment, DNS, and certificates

The image uses official Tailscale `containerboot` v1.102.4 [6]. Empty `TS_STATE_DIR` and `TS_KUBE_SECRET` select `--state=mem:` and `/tmp` for ancillary state. `TS_USERSPACE=true` removes the kernel TUN dependency.

The auth key must be reusable, ephemeral, preauthorized, short-lived, and limited to `tag:cf-proof`. Reuse is necessary because every cold start creates a node. `TS_AUTH_ONCE=false` causes enrollment on each process start. The key lives only in Cloudflare Worker Secrets and the Tailscale process environment. The supervisor removes it from the HTTP child environment.

Ephemeral nodes normally disappear 30–60 minutes after inactivity [8]. Memory-state clients attempt logout on clean daemon exit. A crash or KILL can leave stale nodes during that interval. Cleanup is not instantaneous or guaranteed at a fixed deadline.

`TS_CERT_DOMAIN` substitutes the certificate domain supplied by Tailscale into Serve configuration [6]. The client requires MagicDNS and HTTPS support in its Tailnet. Serve obtains the HTTPS certificate. Funnel remains disabled.

The requested hostname is `cf-tailnet-proof`. Repeated starts can create suffixes while old nodes remain. The IP also changes. A stable name is not an acceptance assumption. The live test must record the actual FQDN and certificate after each restart. Certificate Transparency can disclose machine names [11]. Neutral names reduce that disclosure. Repeated certificate issuance can also encounter CA limits.

## Threat model and residual exposure

| Threat | Control | Residual risk |
|---|---|---|
| Public inference/admin access | No default public routes. DO `fetch()` always denies. No forwarding of request URLs, bodies, or container ports. | A later configuration or code change can reintroduce a route. Account-wide route audit remains necessary. |
| Forged wake | HMAC-SHA256 with a random 256-bit secret, HTTPS, origin/action binding, ±30-second timestamps. | Worker parsing and signature verification remain publicly callable after route approval. |
| Replay/race | SQLite DO transaction consumes each nonce. Retention includes allowed future clock skew. | Replay storage must persist. Migration or rollback must not erase it while signatures remain valid. |
| Resource exhaustion | One instance. Six wake calls and sixty control calls per fixed minute. Unauthenticated requests never reach the DO. | Edge invocation cost remains. Fixed windows permit boundary bursts. Stolen keys can keep the instance active. |
| Tailnet lateral movement | Dedicated tag, least-privilege grants, no exit node, no routes, no Funnel. | Existing broad ACLs are additive. A new narrow grant does not cancel them. |
| Credential leakage | Secrets excluded from image/build context. No raw child logs. No body/header logging. | Cloudflare administrators and a compromised container can access process secrets. |
| Data loss and stream truncation | Explicit wake lease while active, graceful shutdown, external persistence planned. | Platform eviction can still interrupt streams. Exactly-once inference is not provided. |
| Public database access | Stage 2 blocked until a private Neon path exists. | Public TLS endpoints and IP allowlists do not satisfy the strict topology. |

The prototype contains no prompts, accounts, provider keys, database connections, or model responses. Its public lifecycle response contains only a status enum. Cloudflare and Tailscale control-plane availability remain dependencies. DERP can observe traffic timing and volume, not decrypt WireGuard payloads [10].

## Costs: no measured Cloudflare estimate yet

No Cloudflare deployment occurred. Active time, idle time, CPU, relay latency, and egress volume are **unmeasured**. Local image size was 147,597,245 bytes. This is not a Cloudflare billing measurement.

Cloudflare lists these rates on 2026-09-23 [12]:

- Workers Paid: $5/month base
- Memory: $0.0000025 per GiB-second after 25 GiB-hours/month
- CPU: $0.000020 per active vCPU-second after 375 vCPU-minutes/month
- Disk: $0.00000007 per provisioned GB-second after 200 GB-hours/month
- Egress: $0.025/GB in North America/Europe, $0.05 in specified Asia/Oceania regions, $0.04 elsewhere, after regional allowances.

The prototype uses `basic`: 1 GiB RAM, 0.25 vCPU, and 4 GB disk. Before allowances, one active hour costs $0.010008 for RAM/disk, plus up to $0.018 for fully utilized CPU. At 730 active hours, the upper-bound resource estimate is about $24.72 including the $5 base and listed allowances. This is an illustrative bound, **not a measured quote**. Workers, DOs, logs, egress, Tailscale, Neon, and an AWS connector can add charges.

Measured billing estimate after the test:

```text
RAM  = max(0, active_seconds × 1 - 90,000) × 0.0000025
CPU  = max(0, measured_vcpu_seconds - 22,500) × 0.000020
Disk = max(0, active_seconds × 4 - 720,000) × 0.00000007
Total = $5 + RAM + CPU + Disk + other services and egress
```

Allowances are account-wide. A heartbeat during every active session can preserve streams, but increases active time. Repeated wakes can remove most scale-to-zero savings.

## Executed offline evidence

The final offline run on 2026-09-23 produced these results:

| Command in `prototype/` | Result |
|---|---|
| `npm test` | 11 passed, 0 failed. Includes twenty concurrent replays against SQLite and replay after runtime restart. |
| `npm run check` | TypeScript exited 0. |
| `python3 -m unittest discover -s test -p '*_test.py' -v` | 2 passed. Health/unknown paths and ordered SSE with client disconnect. |
| `docker build --platform linux/amd64 -t tailnet-proof:local .` | Image built successfully from pinned official manifests. |
| `bash scripts/check-container.sh` | PASS: amd64, unprivileged, no TUN/capabilities, fail-closed enrollment, loopback HTTP, SIGTERM, no raw auth logs. |
| `WRANGLER_SEND_METRICS=false npm run dry-run` | Image build and Worker bundling succeeded. Output: `--dry-run: exiting now.` Nothing uploaded. |

BuildKit warns about the names `TS_KUBE_SECRET` and `TS_AUTH_ONCE`. Their values are an empty string and `false`, not credentials. `TS_AUTHKEY` is absent from the image configuration.

These checks do not prove Tailnet enrollment or private reachability. The Miniflare test mocks container lifecycle, but uses real local Durable Object storage. Local short SSE checks do not prove the ten-minute Cloudflare stream.

## Stage 2 revision and validation gate

The source Puck thread reports later unpushed Codex work. Its latest reported local `main` is recorded in the [Codex thread](https://ampcode.com/threads/T-01a0c98a-7aa7-724e-8498-61c6d7797416). The reported tip is `78af9e3d8df365c7b45e31d2a5ccc37ab5be4b55`, not necessarily the current tip. No source files were imported or reverted here.

After a live pass, obtain a fresh bundle and exact tree from that thread. Verify ancestry before integration. The old `codex-live.bundle` does not necessarily contain the latest work. Rebuild the native Headroom plugin and Bifrost with one exact Go workspace, toolchain, and dependency graph.

Stage 2 remains untested for all of these requirements:

- Neon migrations, config and credential persistence, pooled verified TLS, and cold database recovery
- Owner/account isolation, Codex refresh fencing, disconnect cleanup, and restart recovery
- Separate Tailnet admin/inference grants plus Bifrost application authentication
- Official Python Headroom with beacon/telemetry disabled and loopback-only access
- Chat/Responses unary and SSE, tools, reasoning, structured output, and OAuth UI/device flow
- Headroom compression/bypass metrics and ephemeral learned-state boundaries
- Disabled CCR, answer/semantic caching, and unsafe learned-state sharing, with provider prefix caching preserved
- Separate Cloudflare encryption-key secret, offline recovery copy, and tested restore procedure.

## Primary sources

All documentation was fetched live on 2026-09-23. Upstream repositories were read-only.

1. [Cloudflare Container architecture](https://developers.cloudflare.com/containers/concepts/architecture/)
2. [Container class reference](https://developers.cloudflare.com/containers/reference/container-class/) and [low-level DO Container API](https://developers.cloudflare.com/durable-objects/api/container/)
3. [SDK source at npm 0.3.7 gitHead](https://github.com/cloudflare/containers/blob/298169f4aaba82e7b712458b7c6b14fc3e40ad78/src/lib/container.ts): `renewActivityTimeout`, `fetch`, `start`, and `startContainer`.
4. [Cloudflare FAQ](https://developers.cloudflare.com/containers/faq/)
5. [Cloudflare outbound traffic](https://developers.cloudflare.com/containers/guides/outbound-traffic/)
6. [Tailscale Docker parameters](https://tailscale.com/docs/features/containers/docker/docker-params), [v1.102.4 containerboot](https://github.com/tailscale/tailscale/tree/v1.102.4/cmd/containerboot), and [userspace networking](https://tailscale.com/docs/concepts/userspace-networking)
7. [Tailscale workload identity federation](https://tailscale.com/docs/features/workload-identity-federation)
8. [Tailscale ephemeral nodes](https://tailscale.com/docs/features/ephemeral-nodes)
9. [Neon Private Networking](https://neon.com/docs/guides/neon-private-networking). The page mixes older Business and current Scale wording. Verify account eligibility before purchase.
10. [Tailscale connection types](https://tailscale.com/kb/1257/connection-types)
11. [Tailscale HTTPS certificates](https://tailscale.com/kb/1153/enabling-https)
12. [Cloudflare pricing](https://developers.cloudflare.com/containers/platform/pricing/) and [instance limits](https://developers.cloudflare.com/containers/platform/limits/)
