# NixOS deployment

The monorepo imports `nix/modules/bifrost.nix` and `deploy/nixos/module.nix`.
It supplies the Buck-built package through `services.bifrost.package`.

## Network boundary

Bifrost listens only on `127.0.0.1:8080`. Caddy exposes two loopback listeners:

- `127.0.0.1:8081`: authenticated inference routes only.
- `127.0.0.1:8082`: dashboard and authenticated inference.

Tailscale Serve publishes the dashboard listener as the private `svc:ai`
service on HTTPS 443 and 8443.
Optional Tailscale Funnel publishes only the inference listener. The ash host
keeps Funnel disabled. Its public inference endpoint is a Cloudflare Tunnel to
the same inference listener, configured by the monorepo in `src/ai/service.nix`.

No application port is opened by the host firewall. Caddy permits only POST
requests to `/v1/chat/completions`, `/v1/responses`, and `/v1/messages` on the
inference listener. Bifrost validates virtual keys and their permissions.

## Runtime configuration

```nix
services.bifrostDeployment = {
  enable = true;
  publicInference = false;
  environmentFile = "/run/secrets/bifrost.env";
  redisPasswordFile = "/run/secrets/valkey-password";
};
```

The environment file supplies the pooled Neon host, database owner credentials,
the Bifrost encryption key, and the administrator password. Neon uses TLS
`verify-full`. Bifrost runs its normal startup migrations as the owner of its
dedicated database. Keep the encryption key stable across upgrades and restores.

The Valkey password file contains exactly 64 hexadecimal characters. The module
runs native Valkey with Search on loopback, disables persistence, and gives it a
dynamic systemd user. Valkey is an ephemeral cache; Neon is the durable store.

Headroom and semantic embeddings remain optional. When enabled, their Modal app
name and credentials are supplied through runtime configuration and the native
plugin loaded from the Buck package.

## Validation

Evaluate the reusable module with:

```sh
nix eval --impure --json --file deploy/nixos/eval-test.nix
```

The monorepo also evaluates the composed ash service through its `ai-service`
check. Package tests require a Buck package path in `BIFROST_PACKAGE`; the cache
suite additionally requires the monorepo vendor output and a local container
daemon for its disposable test cache.
