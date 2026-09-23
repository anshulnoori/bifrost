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
The Valkey password file must contain only its password. Both files require mode 0600.
The NixOS Redis module reads its password file through its privileged pre-start process.

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

This profile supplies plain Valkey, not a verified Search module. Bifrost does not yet use it for semantic caching.
Search module packaging, ARM compatibility, and Bifrost vector-query tests remain required work.
Disposable cache data has no disk persistence. Learned memory needs a separate durable storage policy.
The profile does not silently enable an incompatible cache backend.

## Modal and managed-service privacy

The existing Headroom plugin builds with the gateway but remains disabled in this profile.
The old public Modal ASGI facade does not meet a strict tailnet-only requirement.
The release needs either tailnet networking inside Modal or a private Modal function with a local authenticated bridge.
The latter still uses Modal's authenticated control plane, rather than tailnet transport.
GPU-capable compression, CUDA dependencies, warm/cold benchmarks, and CPU fallback tests remain unfinished.

Neon is also outside the tailnet. TLS and passwords are not tailnet network isolation.
Before production, approve an appropriate Neon network restriction or an explicit managed-service exception.
A VM egress-IP allowlist is narrower than public access, but it is not tailnet identity enforcement.

## Validation before public activation

From this repository, run the local checks:

```bash
nix build .#bifrost-stack
nix eval --impure --json --file deploy/nixos/eval-test.nix
nix develop .#deployment --command node --test deploy/nixos/edge.test.mjs
BIFROST_PACKAGE=$(readlink -f result) nix develop .#deployment --command node --test deploy/nixos/package.test.mjs
```

The edge test requires Caddy 2.11.4 from the locked nixpkgs input in `PATH`, or its path in `CADDY`.
Debian's Caddy 2.6.2 panics on the positive header allowlist. It is not a supported test binary.
The suite uses an actual Caddy process and synthetic upstream responses, not live provider credentials.

Local results: the x86-64 Nix package built and loaded its native Headroom plugin.
The package smoke test passed authentication rejection, UI asset delivery, and clean SIGTERM shutdown.
All seven edge tests passed, including streaming, cancellation, credential filtering, and both sides of the body-size limit.
The ARM NixOS system evaluated successfully. This is not an ARM boot or runtime test.
The Headroom race suite also passed. No provider credentials or live cloud resources participated.

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
