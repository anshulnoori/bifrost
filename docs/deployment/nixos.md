# NixOS is the active deployment target

This deployment replaces the Cloudflare Workers/Containers plan. The old code remains for history and regression tests.
This profile is not a live deployment or a complete GPU/cache release.

## Network boundary

```text
Internet -> Tailscale Funnel :443 -> Caddy 127.0.0.1:8081 -> Bifrost 127.0.0.1:8080
Tailnet  -> Tailscale Serve :8443 -> Caddy 127.0.0.1:8082 -> same Bifrost
                                                          |
                                                          +-> Neon (verified TLS)
Local cache clients -> authenticated Valkey 127.0.0.1:6379
```

Funnel provides public HTTPS on the node's `*.ts.net` hostname. Caddy provides the route and header boundary, not public TLS.
Admin uses the same hostname on port 8443, through private Serve. No admin route exists on the public listener.
The OIDC callback must use the exact private origin: `https://NODE.TAILNET.ts.net:8443/api/session/oidc/callback`.
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
6. Enroll the dedicated node in Tailscale through an owner-operated login.
7. Merge the example tailnet policy, then test its effective permissions.
8. Restart `bifrost-admin-serve` after enrollment.

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
Codex still bypasses this cache: a hit must not bypass subscription admission or survive an account disconnect.
Embedding-based semantic matching is not yet enabled. It requires an explicit embedding model and matching index dimension.
Changing dimension requires a new namespace; changing embedding models also requires a new namespace even at the same dimension.
The SDK must register governance before semantic cache when using `scope_by_virtual_key`.
Disposable cache data has no disk persistence. Learned memory needs a separate durable storage policy.
Valkey's vector indexes consume memory in addition to cache payloads. Monitor RSS and evictions before increasing the 2 GiB budget.

## Modal and managed-service privacy

The Headroom plugin builds with the gateway. Set `services.bifrostDeployment.headroomEndpoint` only after validating the selected Modal endpoint.
Human administration remains tailnet-only. The owner permits authenticated outbound public connections to managed services.
Modal service-to-service requests therefore need authentication, but do not require a browser-accessible admin endpoint.
The existing ASGI facade requires Modal proxy authentication and a separate service credential; it exposes no dashboard.

`deploy/modal/headroom/app.py` defines separate CPU and L4 GPU endpoints, each with at most two containers and scale-to-zero.
The GPU image pins CUDA PyTorch dependencies and both Kompress and ModernBERT model revisions.
Startup explicitly selects PyTorch and CUDA, verifies a GPU forward pass, then serves requests. ONNX CPU fallback cannot masquerade as CUDA.
A killable subprocess retains model weights between requests. It receives no Modal or service credentials and uses offline model files.
Cancellation discards that worker. The next request starts a fresh worker. Request and response caches, CCR, and learned memory remain disabled.
Those features require tenant-scoped persistence and retrieval support that the current bridge does not provide.

Add these runtime environment variables when enabling Headroom:

- `HEADROOM_PROXY_TOKEN`: Service bearer shared with Modal's `bifrost-headroom` secret.
- `HEADROOM_SCOPE_KEY`: Independent HMAC secret for request scoping.
- `HEADROOM_METRICS_TOKEN`: Independent credential for loopback-only metrics.
- `HEADROOM_MODAL_KEY` and `HEADROOM_MODAL_SECRET`: Modal **proxy auth tokens**, not account API credentials.

The first three values must each contain at least 32 characters. Supply them through the secret manager, never shell arguments.
The gateway compresses only eligible tool text of at least 4 KiB. Its 500 ms timeout preserves the original input on failure or cold start.
Metrics remain on loopback port 9909. To disable compression, set `headroomEndpoint = null` and restart the gateway.
To use the CPU alternative, select its separately deployed origin. This is an operator rollback, not an automatic cross-endpoint retry.

The following commands create remote resources and incur charges. Run them only after approving the deployment and supplying credentials privately:

```bash
uv run --with modal==1.5.5 modal deploy deploy/modal/headroom/app.py
# Set HEADROOM_BENCH_URL privately to the corresponding endpoint for each run.
python deploy/modal/headroom/benchmark.py --live --backend cpu --samples 20
python deploy/modal/headroom/benchmark.py --live --backend cuda --samples 20
```

Each benchmark run sends 60 synthetic requests. Reports contain timings and counts, not prompts or credentials.
Run from the Oracle VM to include network latency. Record confirmed cold starts separately; the script cannot infer Modal placement.
Initial acceptance targets are warm p95 below 200 ms, at least 20% token reduction, and no loss of required facts in representative fixtures.
Compare task success and full provider latency against bypass and CPU before selecting CUDA. A single retained synthetic fact is insufficient.
No GPU latency improvement, GPU runtime success, or net cost saving has been measured in the orb.

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
The wire harness also includes `vk-cache-isolation`; it requires two owner-supplied test virtual keys and an enabled scoped cache.
The index-dimension guard is startup-only and is covered by direct adapter tests rather than an inference request.

The cache editor preserves `scope_by_virtual_key` when saving other fields. Mock-only browser tests cover both values:

```bash
# Run from tests/e2e with the UI development server already running.
BASE_URL=http://127.0.0.1:3107 npx playwright test --config playwright.cache.config.ts
```

The Modal application imports with SDK 1.5.5 without authentication or remote deployment.
Twelve Python tests cover facade authentication, body limits, CPU fidelity, disconnect cleanup, and mocked CUDA selection.
These tests do not validate a CUDA driver, the GPU image, model downloads, or GPU performance.

Before activation, validate these host conditions:

- The native ARM binary loads the matching Headroom plugin.
- The restricted Neon role starts after migration and survives a restart.
- The private dashboard and native OIDC callback work from an authorized tailnet client.
- A device outside the tailnet cannot connect to admin port 8443.
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
