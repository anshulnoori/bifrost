# Experimental Headroom integration

This integration is **not production-ready**. It provides marker-free tool-result compression through the official Headroom service. It does not implement CCR.

The native Bifrost plugin runs after governance and routing (`post_builtin`). Headroom remains the only compression implementation. No Go compressor or provider proxy exists here.

## Supported boundary

The plugin runs after governance admission. `scope: "gateway"` applies to all admitted requests in a single-tenant gateway.
`scope: "restricted"` requires a matching project or virtual key ID. Omitting `scope` preserves restricted behavior.
Existing caller identities remain separate attribution partitions in either mode. Unauthenticated requests share the explicitly configured gateway scope.
The plugin uses Bifrost's resolved session ID, including supported client session headers or `x-bf-session-id`.
`x-headroom-thread` optionally separates threads within a session. Otherwise, the session ID is also the thread label.
Requests without a session use their request ID as a separate marker-free compression partition.
These labels cannot grant access or replace the authenticated project or virtual key.

Only text tool results cross the sidecar boundary. System prompts, tool definitions, reasoning, strict schemas, and multimedia remain in Bifrost.
The plugin patches selected text fields without changing admitted model, provider, account, tool IDs, or request order.

| Lane | Behavior |
| --- | --- |
| Typed Chat and Responses | Compress eligible text tool results, including streaming requests |
| Raw Chat, Responses, Anthropic POST, including SSE | Compress known string tool-result fields; preserve response bytes |
| Unknown endpoints, query parameters | Bypass |
| Provider-managed state or explicit prefix cache controls | Bypass |
| Multimedia tool results | Preserve unchanged |
| Missing governance/configured scope/session/thread | Bypass |
| CCR or internal retrieval obligations | Reject configuration or sidecar response |

Responses and SSE chunks remain unchanged. The plugin never executes client tools or makes provider calls.
Compression failure preserves the original request in `open` mode. In `closed` mode, failure returns 503 without fallbacks.
Cancellation also stops the compression request. No compression-result memoization or semantic answer cache exists in this plugin.

## Local build and configuration

Use the same Go toolchain, dependency versions, build flags, and platform for Bifrost and its native plugin.
Go plugins cannot load reliably into an unrelated released Bifrost binary.

From the repository root:

```sh
cd integrations/headroom
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
GOWORK=off go build -buildmode=plugin -o /tmp/headroom.so .
uv venv /tmp/headroom-venv --python 3.11
uv pip install --python /tmp/headroom-venv/bin/python --require-hashes -r requirements.lock
export PATH="/tmp/headroom-venv/bin:$PATH"
# Supply distinct private values through your secret manager, each >=32 bytes:
# HEADROOM_PROXY_TOKEN, HEADROOM_SCOPE_KEY, HEADROOM_METRICS_TOKEN
sh run-sidecar.sh
```

`requirements.lock` pins transitive Python dependencies with hashes for Python 3.11. This is a development setup, not a production image.
In an Amp orb, run the sidecar through `amp orb service start`, not a background shell.

Register `/tmp/headroom.so` as a native custom plugin in Bifrost. Select `post_builtin` ordering.
Copy `config.example.json` into its configuration. Replace `project_id` with the authenticated governance project ID.
Set `enabled` to true only for an evaluation project. Keep credentials in environment variables, not plugin configuration.

The HTTP server requires this registration beside the other management handlers:

```go
handlers.NewHeadroomHandler().RegisterRoutes(s.Router, middlewares...)
```

The Codex integration owns this registration. This patch intentionally does not modify `server.go`.
Open **Headroom** in the sidebar after the UI build. Save the configuration on that page.
Choose **All admitted requests on this gateway** to use normal provider configuration, including Codex, without a virtual key.
Optional virtual-key restrictions use the ID from Governance, not the virtual key secret.
An optional project ID further restricts the scope. Both must match when both are configured.
New native-plugin installation requires dashboard administrator authentication. Existing configuration saves do not change the installed plugin path.
The page requires `HEADROOM_METRICS_TOKEN` even when OSS dashboard authentication is disabled.
`BIFROST_HEADROOM_METRICS_PORT` defaults to 9909. The handler always connects to numeric loopback, never a caller-supplied URL.

For the combined Codex/Headroom orb, prepare the sidecar from the repository root:

```sh
bash integrations/headroom/prepare-local.sh
make setup-workspace
go work use ./integrations/headroom
go build -o /tmp/bifrost-codex ./transports/bifrost-http
go build -buildmode=plugin -o /tmp/headroom-combined.so ./integrations/headroom
```

Start the gateway with `/tmp/bifrost-headroom-runtime/environment` sourced in its managed service command.
The preparation script generates private local credentials without printing them. It does not change provider credentials or restart the gateway.
The monitoring token comes from `HEADROOM_METRICS_TOKEN` in that private environment file.

The full-path local test uses the real gateway, native plugin, and sidecar with a fixture provider:

```sh
source /tmp/bifrost-headroom-runtime/environment
HEADROOM_GATEWAY_BINARY=/tmp/bifrost-codex HEADROOM_PLUGIN_PATH=/tmp/headroom-combined.so \
HEADROOM_BENCH_URL=http://127.0.0.1:8787 HEADROOM_BENCH_TOKEN="$HEADROOM_PROXY_TOKEN" \
go test -run TestGatewayWithLiveHeadroom -v ./integrations/headroom
```

## Privacy, retention, and deployment limits

Run a dedicated Headroom process for each project. Do not expose its listener publicly.
Use network policy to deny unnecessary egress. The wrapper disables external beacon and local telemetry unless explicitly enabled through environment variables.
`--stateless` disables disk state. It does not prove isolation of all in-memory learned statistics between threads.
**Strict thread isolation is unresolved. Do not use this deployment for sensitive multi-thread workloads.**

The plugin stores at most 1,000 metadata events per replica. `retention_seconds` accepts 0 through 86400.
Zero disables event storage. Restart removes events. Metrics contain finite outcome labels, never project or thread labels.
The private snapshot includes provider, model, project, and opaque principal/thread hashes. It contains no prompt text or credentials.
Disable upstream request logging separately if prompt retention is prohibited. This plugin does not change Bifrost logging policy.

The HMAC scope is an attribution partition, not a CCR capability. No CCR handles exist in this implementation.
There is no shared event database, durable turn state, migration, affinity router, or restart recovery.
Monitor configuration changes require a gateway restart. Drain active requests before plugin removal or restart.
The monitor listener lives until gateway exit because native-plugin reloads share package state.
Removing the plugin stops new hook events. Restart the gateway to close the listener immediately.

## Accounting and evaluation

The snapshot preserves provider-reported usage separately from estimated tool-result token counts.
Provider usage can include input, cache-read, cache-write, output, and reasoning fields when the provider reports them.
Missing usage stays unknown. Estimated token reduction is not actual cost savings.

Hooks observe primary and fallback attempts, not each internal network retry. Intermediate retry usage and cost remain unknown.
Compression latency and plugin-attempt duration are measured. TTFT and total multi-turn task latency are not measured here.
Quality remains `not_evaluated`. The dashboard never reports zero quality loss.

The local fixture compares identical input with compression disabled and enabled:

```sh
HEADROOM_BENCH_URL=http://127.0.0.1:8787 \
HEADROOM_BENCH_TOKEN="$HEADROOM_PROXY_TOKEN" \
GOWORK=off go test -run TestLiveHeadroomFixture -v
```

A measured run preserved a planted FATAL transaction fact: 16,527 → 395 bytes and 5,272 → 145 estimated tokens.
With the checked-in wrapper, the disabled plugin took 4 microseconds. Compression took 498,629 microseconds.
This run used Headroom 0.38.0 with stateless mode, CCR disabled, semantic cache disabled, and the hashed dependency lock.
It is not a provider benchmark or a CCR comparison. It measures neither answer correctness nor billed usage.

## Rollback and production gates

Set `enabled` to false to stop transformations. If listener configuration prevents reload, restart the gateway with the plugin disabled.
Remove the native plugin after active requests finish. Remove the sidecar and its credentials when no consumers remain.
No database migration is necessary because this patch adds no durable tables.

Production remains blocked by:

- Strict isolation of Headroom learned state across threads.
- Scoped CCR capabilities, expiry, durable continuations, affinity, and bounded safe redrive.
- Complete provider-attempt accounting and cost attribution.
- Durable metadata storage and HA aggregation.
- End-to-end Codex OAuth and real-provider protocol harness coverage.
- Representative paired answer-quality and latency evaluations.
- A production image and native ABI compatibility verification with the final combined gateway.

See `RESEARCH.md` for the source evidence behind the architecture change.

## Dashboard fixture verification

`testdata/dashboard.json` contains synthetic provider usage and timing data. It is not measured provider evidence.
For a browser-only preview, intercept `/api/config?*` with `{"is_db_connected":true}`.
Intercept `/api/headroom/events` with this fixture. Use a local UI production preview so management requests use the same origin.
Exercise the empty, unavailable, populated, filtered, and cleared states. Compare the estimate card with the separate native usage column.
