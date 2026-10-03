# Headroom integration

This native Bifrost plugin compresses eligible tool results through the private
Modal app in `deploy/modal/headroom`. It also proxies MiniLM embeddings to that
app for the semantic cache. The production transport is the Modal Go SDK; the
deployment does not expose a public HTTP endpoint.

## Production path

1. Bifrost loads `headroom.so` and validates its configuration.
2. The plugin admits only requests with settled governance identity and limits.
3. Eligible tool text is sent to `bifrost-headroom.Headroom.request` by the
   private Modal SDK transport.
4. Modal's `app.py` creates the GPU compressor and the in-process ASGI service.
5. `gpu.py` talks to the long-lived framed worker in `gpu_worker.py`.
6. The gateway validates the returned message shape before changing a request.

The plugin fails open by default. Set `failure_policy` to `closed` only when a
compression failure must reject the provider request.

## Deployment

Populate the immutable model volume once, then deploy the app:

```sh
modal run deploy/modal/headroom/app.py::prepare_weights
modal deploy deploy/modal/headroom/app.py
```

The app requires the Modal secrets `headroom-dockerhub` (read-only image import)
and `bifrost-headroom` (service state). The runtime image is pinned by digest.
Weights are mounted read-only from `bifrost-headroom-models-v1`.

The gateway environment must provide the variables named by these configuration
fields:

- `modal_key_env` and `modal_secret_env`: Modal API credentials.
- `scope_key_env`: at least 32 bytes, used for tenant-scoped identifiers.
- `metrics_token_env`: at least 32 bytes when the loopback monitor is enabled.

The Nix deployment generates the production configuration. Its active keys are
`enabled`, `embedding_proxy_enabled`, `modal_app`, `modal_environment`, `scope`,
`cache_dir`, `ccr`, `amp_deferred_retrieval_virtual_key_id`, the environment-name
fields above, `failure_policy`, request limits, and monitor settings.

## Isolation and retrieval

Tenant identity comes from the settled governance grant, never request headers.
Project, principal, session, provider, and model are included in scoped IDs.
Compression decisions are encrypted on disk and partitioned by the authenticated
owner. Persisted formats and scope derivation are compatibility boundaries.

CCR is enabled only for authenticated non-gateway principals. The client must
advertise the retrieval tool, or use the configured Amp deferred-retrieval
virtual key. Retrieval is served by the gateway's authenticated Headroom MCP
route; Modal does not retain retrieval state.

## Monitoring

`metrics_address` must be a numeric loopback address. The monitor requires a
bearer token from `metrics_token_env` and serves bounded event history, metrics,
the embedding proxy, and internal retrieval. Listener or token changes require a
gateway restart.

Events contain metadata and token estimates, not prompt or tool-result content.
The event ledger retains at most 10,000 entries and honors `retention_seconds`.

## Tests

```sh
PATH=/usr/local/go/bin:$PATH go test -race ./integrations/headroom/...
python -m unittest discover -s deploy/modal/headroom -p 'test_*.py'
```

Live Modal tests are opt-in through the environment checks in `modal_test.go`.
They are not part of routine unit verification.
