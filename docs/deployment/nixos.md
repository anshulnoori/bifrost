# NixOS is the active deployment target

This deployment replaces the Cloudflare Workers/Containers plan. The old code remains for history and regression tests.
This profile is not a live deployment or a complete GPU/cache release.

## Network boundary

```text
Internet -> node Funnel :443    -> Caddy 127.0.0.1:8081 -> Bifrost 127.0.0.1:8080
Tailnet  -> svc:ai HTTPS :443   -> same inference-only Caddy listener
Tailnet  -> svc:ai HTTPS :8443  -> Caddy 127.0.0.1:8082 -> same Bifrost
                                                          |
                                                          +-> Neon (verified TLS)
Local cache clients -> authenticated Valkey 127.0.0.1:6379
```

The private Tailscale Service is `svc:ai`, with inference on HTTPS port 443 and admin on port 8443.
The requested hostname is `ai.silverside-mongoose.ts.net`, but runner `t1` reports the tailnet suffix `mongoose-silverside.ts.net`.
`ai.mongoose-silverside.ts.net` already resolves there. Confirm the intended tailnet and existing name ownership before creating or reusing the Service.
Funnel provides public HTTPS on the node's separate `*.ts.net` hostname. Its documented CLI has no `--service` flag.
Do not assume the Service hostname can become a public Funnel hostname. Public activation remains a separate step.
Caddy provides the route and header boundary, not public TLS. No admin route exists on its inference listener.
The OIDC callback must use the confirmed Service hostname: `https://ai.<confirmed-tailnet>.ts.net:8443/api/session/oidc/callback`.
The browser must belong to the tailnet. Bifrost still verifies its own session or OIDC login.

Public inference accepts only POST `/v1/chat/completions`, `/v1/responses`, and `/v1/messages`.
The Anthropic route maps to `/anthropic/v1/messages` internally.
Credentials must use a Bifrost virtual key in `Authorization: Bearer`, `x-api-key`, or `x-bf-vk`.
Ambiguous credentials fail. Bifrost, not Caddy, checks key validity, permissions, and budgets.
Provider keys, cookies, identity headers, and routing overrides do not pass through the public proxy.
Query strings, other methods, model listing, WebSockets, and compressed bodies fail closed.
The request limit is 4 MiB. Responses stream without parsing or automatic request replay.
This proxy does not reproduce the old Worker's replay registry or per-IP admission limits.

## Install the profile without exposing inference

The starting host is the owner's Oracle ARM VM: 2 vCPU and 12 GiB RAM.
The host must already have a working NixOS hardware and boot configuration.
This repository does not overwrite disks, invent device names, or configure an Oracle boot image.

1. Import this flake's `nixosModules.deployment` into the host's existing NixOS configuration.
2. Keep the existing hardware configuration, users, SSH keys, and `system.stateVersion`.
3. Enable the profile with this configuration:

```nix
{
  nixpkgs.hostPlatform = "aarch64-linux";
  services.bifrostDeployment = {
    enable = true;
    publicInference = false;
    environmentFile = "/run/secrets/bifrost.env";
    redisPasswordFile = "/run/secrets/valkey-password";
    migrationEnvironmentFile = "/run/secrets/bifrost-migration.env";
  };
}
```

4. Supply runtime secret files through the host's secret manager.
5. Build the configuration before switching the host.
6. Enroll the dedicated node in Tailscale with the `tag:bifrost` identity. Tailscale Services requires a tagged host.
7. In the Tailscale Services console, define `ai` with endpoints `tcp:443` and `tcp:8443`.
8. Merge the example tailnet policy, then test its effective permissions. Approve the host advertisement manually or through the scoped auto-approver.
9. Restart `bifrost-admin-serve` and `bifrost-inference-serve` after enrollment.

The example policy permits tailnet administrators. Existing wildcard grants can permit other members too.
For owner-only access, replace the source group with the owner's identity in the private policy.
Oracle ingress does not need public application ports. Tailscale owns the tunnel and HTTPS listeners.
Do not configure Funnel on 8443 or forward Bifrost port 8080 directly.
Do not reuse a node with unknown persistent Serve/Funnel mappings.

## Secrets and Neon migrations

The root-owned runtime environment file contains these names:

- `NEON_HOST`: The pooled Neon hostname, with `-pooler.` and suffix `.neon.tech`
- `NEON_USER`: The restricted runtime role
- `NEON_PASSWORD`
- `NEON_DATABASE`
- `BIFROST_ENCRYPTION_KEY`: The existing encryption key, not a replacement
- `BIFROST_ADMIN_PASSWORD`: The recovery password, at least 32 characters.

Optional OIDC variables follow [dashboard-oidc.md](../dashboard-oidc.md).
Never put secret values in the flake, CLI arguments, Git, screenshots, or Nix store.
The Valkey password file must contain exactly 64 hexadecimal characters, optionally followed by a newline.
Both files require mode 0600. Systemd credentials supply the same password to Valkey and Bifrost.
Passwords do not appear in process arguments or the Nix store.

The separate migration environment file contains `NEON_DATABASE_URL` for the migrator role.
It requires the direct endpoint, not the pooled endpoint, with `sslmode=verify-full`.
The reusable migration executable comes from the preserved launcher source. It does not invoke Cloudflare.

After approval and a backup, run `sudo systemctl start bifrost-migrate` on the host.
Apply [runtime-role.sql](../../deploy/neon/runtime-role.sql) through an approved database session.
Then start Bifrost with the restricted runtime credentials.
The migration unit never runs at boot. NixOS rollback does not undo database migrations.
Before an OIDC downgrade, revoke sessions as documented in the OIDC guide.

## Memory and cache status

Valkey uses `maxmemory 2gb`, LFU eviction, and a 3 GiB systemd memory limit.
Bifrost uses a 4 GiB soft limit and a 6 GiB hard limit.
These limits are initial budgets, not measured capacity guarantees.

NixOS manages a pinned official Valkey Bundle container through Podman. The bundle contains Valkey 9.1.1 and Search 1.2.1.
Only Search loads. The container runs as UID 999, without capabilities, with a read-only root and no published ports.
Host networking permits only its configured loopback listener. Separate ARM64 and AMD64 image digests are pinned in the module.
Bifrost waits for authenticated Search readiness before starting.

Exact response caching is enabled with a five-minute TTL. Cache keys include the authenticated virtual-key ID.
Caller-supplied cache labels cannot cross that boundary; requests without a governance identity bypass the cache.
Codex cache keys also include the authorized account pool and each account's connection ID.
The gateway checks current authorization before cache lookup, before replay, and before storage.
Account disconnects, connection replacements, and changes to the authorized pool prevent reuse of the previous scope.
Cache hits do not consume upstream subscription quota. The resolver does not read credentials or refresh OAuth tokens.
Setting `semanticCacheEndpoint` enables semantic matching with `headroom-minilm-v1`, 384 dimensions, and a 0.98 cosine threshold without enabling compression.
Set it to `https://anshulnoori--bifrost-headroom-gpu-service.modal.run` for the validated ONNX CPU service.
The selected service uses pinned MiniLM weights. Inputs longer than 256 tokens bypass semantic matching rather than silently losing their suffix.
This threshold is an initial policy, not proof that similar prompts have equivalent answers. Test representative questions before public activation.
The embedding provider calls the authenticated loopback Headroom bridge. Only that bridge holds the Modal proxy credentials.
Embedding requests have a two-second timeout, independent of the 500 ms compression timeout. Embedding errors fall through to the model.
The isolated compression endpoint (`service`) cannot provide semantic hits. The full model endpoint (`gpu_service`) provides embeddings on CPU or GPU.
Changing dimension requires a new namespace; changing embedding models also requires a new namespace even at the same dimension.
The SDK must register governance before semantic cache when using `scope_by_virtual_key`.
Disposable response cache data has no disk persistence. Headroom state uses a separate encrypted PostgreSQL schema, not Valkey.
Valkey's vector indexes consume memory in addition to cache payloads. Monitor RSS and evictions before increasing the 2 GiB budget.

The deployment sets `provider_defaults.prompt_cache.auto_inject = true` globally and enables the Codex prompt-cache policy explicitly.
A provider's explicit `prompt_cache` configuration overrides the global default, including `auto_inject = false`.
Provider capability checks still control cache markers. Codex forwards caller-supplied prompt-cache parameters without an invented enable flag.
This configuration does not guarantee an upstream cache hit. Provider usage reports determine whether upstream caching occurred.

## Modal and managed-service privacy

The Headroom plugin builds with the gateway. Set `services.bifrostDeployment.semanticCacheEndpoint` for embeddings. Leave `headroomEndpoint = null` until compression passes latency and billing acceptance.
Human administration remains tailnet-only. The owner permits authenticated outbound public connections to managed services.
Modal service-to-service requests therefore need authentication, but do not require a browser-accessible admin endpoint.
The existing ASGI facade requires Modal proxy authentication and a separate service credential; it exposes no dashboard.

`deploy/modal/headroom/app.py` defaults to CPU. `HEADROOM_ACCELERATOR=T4` or `L4` explicitly selects GPU hardware for `gpu_service` at deployment time.
Each endpoint uses zero minimum containers, one maximum container, no warm buffer, one concurrent input, and a 30-second scale-down window.
The `service` endpoint uses isolated compression. The `gpu_service` endpoint uses ONNX CPU unless GPU hardware is explicitly selected.
The full model image pins dependencies and Kompress, ModernBERT, and MiniLM model revisions.
CPU compression selects ONNX Runtime and the pinned `onnx/kompress-int8-wo.onnx` model. Startup rejects a PyTorch compression fallback. MiniLM embeddings still use PyTorch on CPU.
Models load before readiness. CPU reports `onnx-cpu-kompress-v2`; CUDA reports `cuda-kompress-marker-free-v1` after a GPU forward pass.
A killable subprocess retains model weights between requests. It receives no Modal or service credentials and uses offline model files.
Cancellation discards that worker. The next request starts a fresh worker.
Answer caching belongs to Bifrost/Valkey; the Headroom worker does not maintain a second response cache.
The facade provides durable CCR retrieval and the official Headroom memory tools, with tenant-scoped encrypted storage.
**Gateway integration of these internal tools remains unfinished.** The configured gateway still sends marker-free compression and rejects `ccr = true`.
Do not change that flag until bounded internal execution, final-response checks, and cache invalidation are implemented and tested together.

Add these runtime environment variables when enabling Headroom:

- `HEADROOM_PROXY_TOKEN`: Service bearer shared with Modal's `bifrost-headroom` secret.
- `HEADROOM_SCOPE_KEY`: Independent HMAC secret for request scoping.
- `HEADROOM_METRICS_TOKEN`: Independent credential for loopback-only metrics.
- `HEADROOM_MODAL_KEY` and `HEADROOM_MODAL_SECRET`: Modal **proxy auth tokens**, not account API credentials.

The GPU service also requires two values in its private Modal secret, never on the gateway:

- `HEADROOM_DATABASE_URL`: A separate Headroom database owned by the `headroom` application role, with `sslmode=verify-full` on a Neon hostname. Set `sslrootcert` to the runtime's trusted CA bundle.
- `HEADROOM_STATE_KEY`: An independent 32-byte AES key encoded as 64 hexadecimal characters. Do not reuse Bifrost's encryption key.

Before deploying that service, create the dedicated database and role through the owner's authenticated Neon session.
Apply [headroom-state.sql](../../deploy/neon/headroom-state.sql) as `headroom`. This single role owns the database, schema, and tables and can run future migrations. The application never creates schemas at startup.
Keep its `NEON_HOST`, `NEON_DATABASE`, `NEON_USER`, and `NEON_PASSWORD` in a separate Headroom 1Password Environment. Assemble the connection string when provisioning the Modal secret; do not add these credentials to Bifrost's Environment.
CCR originals expire after 30 minutes; learned memories and replay journals expire after seven days.
Each scope has a 32 MiB encrypted-data limit and a 256-record limit across all state kinds. Exhaustion fails the operation without partial writes.
Schedule the SQL file's expired-record deletion with an owner-managed database job; reads reject expired records before physical deletion.
Deleting a learned memory removes its retained version chain. Mutation journals contain identifiers and digests, not remembered text.
Neon backups have their own retention policy; logical deletion does not erase existing backups.

The first three values must each contain at least 32 characters. Supply them through the secret manager, never shell arguments.
The gateway compresses only eligible tool text of at least 16 KiB. This initial size gate reduces cold-start spending, but does not prove profitability.
Its 500 ms timeout preserves the original input on failure or cold start. Queued requests carry deadlines that prevent expired requests from starting compression.
Metrics and the embedding facade remain on loopback port 9909. To disable compression while retaining semantic caching, set `semanticCacheEndpoint` and leave `headroomEndpoint = null`, then restart the gateway.
To use the CPU alternative, select its separately deployed origin. This is an operator rollback, not an automatic cross-endpoint retry.

The following commands create remote resources and incur charges. Run them only after approving the deployment and supplying credentials privately:

```bash
HEADROOM_ACCELERATOR=T4 uv run --with modal==1.5.5 modal deploy deploy/modal/headroom/app.py
# Set HEADROOM_BENCH_URL privately to the corresponding endpoint for each run.
python deploy/modal/headroom/benchmark.py --live --backend t4 --samples 1 --billable-overhead-seconds 30
```

This example sends three synthetic requests. The 30-second overhead is an explicit idle-time assumption, not measured billing.
Use `--billable-seconds` for a measured total container interval instead. Do not add startup time twice if the measured interval already includes it.
Compare equivalent runs with `--backend cpu`, `t4`, and `l4` against separately approved matching deployments.
Headroom 0.38.0 reports whitespace words in its token fields when CCR is disabled. These counts are not provider billing tokens.
Benchmark cost ratios use those word units and explicitly label them. Failed requests and lost facts receive no credit.
`audit_cost.py` reconciles the T4 validation fixtures with Modal billing exports and counts their original input with `o200k_base`.
It cannot recover compressed BPE counts because the validation report does not retain compressed text.
Monthly estimates require eligible tool-input tokens and cold-start frequency, not total input, output, and cached tokens combined.
Estimates include GPU, two CPU cores, and 4 GiB RAM at [Modal base rates](https://modal.com/pricing), retrieved September 26, 2026.
CPU and RAM charges use the greater of requested resources and actual usage. The configured quantities are not billing ceilings.
Reports contain timings and counts, not prompts or credentials. These estimates exclude image builds, storage, regional premiums, and credits.
Run from the Oracle VM to include network latency. Record confirmed cold starts separately; the script cannot infer Modal placement.
Initial acceptance targets are warm p95 below 200 ms, at least 20% token reduction, and no loss of required facts in representative fixtures.
Compare task success and full provider latency against bypass and CPU before selecting CUDA. A single retained synthetic fact is insufficient.
T4 CUDA/PyTorch validation succeeded in a temporary Modal job. Production end-to-end latency and net cost savings remain unverified.

### Cost admission and billing limits

The gateway allows one outbound Headroom request at a time, with no waiting queue. Excess requests retain their original inference input.
This gate covers compression and embeddings from this gateway process. It does not change Modal's platform queue for other authenticated callers.
Tool results from one inference request share one compression call. Embedding batches contain at most 32 texts.
Cross-request batching remains disabled because it adds delay to the 500 ms inference deadline and complicates tenant isolation.
Pinned model files are baked into the image. No memory snapshot captures service credentials or database connections.
Bodies have a 4 MiB limit. Worker execution has a 25-second limit, with shorter caller deadlines and cancellation where available.
The full-model Modal function has a 90-second timeout. Each idle container can remain allocated for up to 30 seconds before scale-down.
T4 validation measured 13.95 seconds for model loading alone. Baked files remove runtime downloads, but do not eliminate CUDA initialization or weight loading.
A gateway timeout during cold start can occur before the ASGI application receives the request.
Request expiry prevents subsequent compression work; it does not prove that container startup stopped or that billing ceased.
Keep the 30-second scale-down window until measurements justify a change. Do not add a warm pool to hide cold starts.
On failure, inference retains the original input. CPU ONNX remains an operator-selected alternative, not a second paid retry after GPU failure.

The deployment sets `cost_ledger_path` to a private file in the gateway state directory.
Each outbound call reserves $0.05 before dispatch, including failures and cancellations. Atomic writes preserve reservations across restarts.
The proposed alert starts at 200 reservations ($10). The circuit blocks further calls after 500 reservations ($25).
The counter resets at the UTC calendar-month boundary, which can differ from Modal's billing cycle.
Corrupt or unreadable state blocks compression, not inference. Missing state initializes a new counter; do not delete the ledger to clear an alert.
This is a conservative request allowance, **not an invoice cap**. Other callers, image builds, and provider charges can exceed it.

Authenticated `/v1/events` records include `cost_reservation_usd`, `month_cost_reservations_usd`, and `budget_alert` alongside request timings.
Compression emits a warning at the alert threshold. Modal logs include metadata-only active-time estimates and model-load timing.
Active-time estimates exclude scale-down idle time. They are not net savings or complete invoices.

The owner reports an existing $1 [Modal limit](https://modal.com/docs/guide/budgets). Do not create or change provider budget settings.
The local $10/$25 reservation thresholds do not replace, raise, or override that provider limit.
Usage limits and net-spend limits have different credit behavior; the configured limit's scope determines its effect.
Provider budgets and alert delivery remain owner-managed. This code does not configure them or imply external notification delivery.
At a billing alert or cutoff, the operator can disable `headroomEndpoint`; inference continues without compression.
If embeddings use the same GPU endpoint, disable `semanticCacheEndpoint` too to stop those calls.

Neon also uses the approved authenticated outbound path, with verified TLS and restricted database roles.
Use an egress-IP allowlist when supported by the chosen Neon plan. This does not replace database authentication.
Do not expose a local database proxy, Modal management proxy, or credentials through Funnel.

## Validation before public activation

From this repository, run the local checks:

```bash
nix build .#bifrost-stack
nix eval --impure --json --file deploy/nixos/eval-test.nix
nix develop .#deployment --command node --test deploy/nixos/edge.test.mjs
BIFROST_PACKAGE=$(readlink -f result) nix develop .#deployment --command node --test deploy/nixos/package.test.mjs
# Requires Docker or CONTAINER_ENGINE=podman and Go from the project toolchain.
BIFROST_PACKAGE=$(readlink -f result) bash deploy/nixos/test-cache.sh
```

The edge test requires Caddy 2.11.4 from the locked nixpkgs input in `PATH`, or its path in `CADDY`.
Debian's Caddy 2.6.2 panics on the positive header allowlist. It is not a supported test binary.
The suite uses an actual Caddy process and synthetic upstream responses, not live provider credentials.

Local results: the x86-64 Nix package built and loaded its native Headroom plugin.
The package smoke test passed authentication rejection, UI asset delivery, and clean SIGTERM shutdown.
All seven edge tests passed, including streaming, cancellation, credential filtering, and both sides of the body-size limit.
The ARM NixOS system evaluated successfully. This is not an ARM boot or runtime test.
The Headroom race suite also passed. No provider credentials or live cloud resources participated.

The cache suite starts and removes a disposable pinned Valkey container. It tests index dimensions, deletion, filters, vector queries,
and virtual-key isolation through the plugin and actual gateway. It never uses a shared cache or paid provider.
Both exact and semantic modes run against the built native plugin. Synthetic embeddings exercise similarity lookup, tenant isolation, and inference during embedding-service failure.
This verifies integration, not the semantic accuracy or performance of the real embedding model.
The wire harness also includes `vk-cache-isolation`; it requires two owner-supplied test virtual keys and an enabled scoped cache.
The index-dimension guard is startup-only and is covered by direct adapter tests rather than an inference request.

The cache editor preserves `scope_by_virtual_key` when saving other fields. Mock-only browser tests cover both values:

```bash
# Run from tests/e2e with the UI development server already running.
BASE_URL=http://127.0.0.1:3107 npx playwright test --config playwright.cache.config.ts
```

The Modal application imports with SDK 1.5.5 without authentication or remote deployment.
Python tests cover facade authentication, body limits, CPU fidelity, disconnect cleanup, mocked CUDA selection, and embedding validation.
With `HEADROOM_TEST_POSTGRES=1`, they create and remove a disposable loopback database to test restricted-role encrypted state,
cross-scope rejection, expiry, quota rollback, concurrent updates, CCR retrieval, memory replay, and deletion.
These tests do not validate a CUDA driver, the GPU image, model downloads, or GPU performance.

Before activation, validate these host conditions:

- The native ARM binary loads the matching Headroom plugin.
- The restricted Neon role starts after migration and survives a restart.
- The private dashboard and native OIDC callback work from an authorized tailnet client.
- The `svc:ai` advertisement is approved and resolves to the confirmed `ai` Service hostname from a tailnet client.
- A device outside the tailnet cannot connect to the Service or its admin port 8443.
- The node has no other public Funnel mappings.
- Invalid virtual keys fail on the actual gateway.
- Valid synthetic inference streams and cancellation pass through Funnel.
- Oracle security rules and the host firewall expose no direct application ports.

Only after these checks, set `services.bifrostDeployment.publicInference = true` and apply the host configuration.
To withdraw public inference, set it to false and apply the configuration.
For immediate withdrawal, run `sudo systemctl stop bifrost-inference-funnel`.
The service's stop command removes only its port-443 mapping, not the private admin mapping.

Funnel has provider bandwidth limits. Heavy-use capacity requires measurements through the real tunnel.
This single VM is not highly available. Its failure removes gateway and cache availability.
