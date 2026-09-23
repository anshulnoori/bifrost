# Private Cloudflare Container experiment

**Stage 1 is prepared, not live-validated. Stage 2 is blocked.** No paid resources, Tailnet nodes, public routes, or databases were created.

The prototype tests one question: can a Cloudflare Container provide reliable private HTTPS through Tailscale, despite container sleep and ephemeral identity?

The current design requires a separate wake action after sleep. Tailnet traffic does not renew the SDK idle timer. Standard Neon endpoints also fail the strict requirement for no public database access.

- [Source-backed findings and threat model](docs/feasibility.md)
- [Deployment and live validation runbook](docs/runbook.md)
- [Minimal image and Worker](prototype/)

## Offline verification

Prerequisites: Node 22 or later, Python 3, and Docker with Buildx. Run these commands from `prototype/`:

```sh
npm ci --ignore-scripts
npm test
npm run check
python3 -m unittest discover -s test -p '*_test.py' -v
docker build --platform linux/amd64 -t tailnet-proof:local .
bash scripts/check-container.sh
WRANGLER_SEND_METRICS=false npm run dry-run
```

The Docker tests use no network and a deliberately invalid key. The Worker tests use synthetic keys. Neither test enrolls a Tailnet node.

In an Amp orb, `.agents/setup` installs the checksum-verified Buildx binary and locked npm dependencies. If Docker is stopped, start it with:

```sh
amp orb service start docker --command 'sudo dockerd --group root --storage-driver=vfs'
```

This command assumes the standard disposable orb, where the user belongs to the root group. Do not use this daemon configuration on shared machines.

**Do not run `wrangler deploy` without explicit approval.** The default configuration disables routes, `workers.dev`, preview URLs, and scheduled triggers.

No Bifrost or Headroom source is imported here. The latest combined revision must come from the Codex source thread after Stage 1 passes.
