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

## Private Modal deployment

The production configuration uses the Modal Go SDK to call `bifrost-headroom.Headroom.request` in the `main` environment.
The default deployment defines no web functions and publishes no Headroom HTTP URL.
`HEADROOM_HTTP_BENCHMARK=1` temporarily exposes the same service behind Modal proxy authentication for transport comparisons.
The private method accepts compression and embedding requests only. CCR and memory tools are unavailable through this method.
The ASGI transport runs inside the Modal process without a listening socket.

The plugin configuration uses `modal_app` and `modal_environment` instead of `endpoint` and `token_env`.
The production Nix options are `headroomModalApp` and `semanticCacheModalApp`.
Both options must select the same app when both features are enabled.
Local HTTP fixtures remain supported.

The gateway requires four values from its 1Password Environment:

1. `HEADROOM_MODAL_TOKEN_ID`: Modal API token ID, not a proxy token.
2. `HEADROOM_MODAL_TOKEN_SECRET`: the corresponding API token secret.
3. `HEADROOM_SCOPE_KEY`: an independent random value of at least 32 bytes.
4. `HEADROOM_METRICS_TOKEN`: another independent random value of at least 32 bytes.

Import these values through 1Password Desktop. Keep them outside Git, the Nix store, logs, and chat.
The gateway does not receive the Headroom database credentials.
Modal API tokens can have broader permissions than invocation alone. Environment restrictions depend on the workspace RBAC configuration.

The L4 deployment uses PyTorch, not ONNX. It has zero minimum and buffer containers, one maximum container, and a 30-second scale-down window.
The gateway admits one request at a time, with no local queue. Modal can still queue function calls during startup.
Every call carries an expiry time. Expired calls do not compress.
Compression waits at most 500 ms, followed by up to 100 ms for best-effort cancellation. Embeddings have a separate two-second deadline.
Cold starts, idle time, SDK retries, and unsuccessful cancellation can still incur charges.
On failure, inference continues with the original request. The deployment does not change the provider billing limit.

A cold model load took 51.7 seconds during validation on September 26, 2026.
The first deployment probe timed out after 60 seconds, including scheduling.
A second probe allowed 120 seconds and passed in 22.98 seconds, including startup.
It reduced 18,863 bytes to 14,995 bytes and preserved two sentinel facts.
These measurements do not establish warm latency, token savings, or billed cost per token.
Production deadlines remain short, so cold requests can bypass compression even when startup incurs charges.

GPU memory snapshots are enabled with `enable_memory_snapshot=True` and `enable_gpu_snapshot=True`.
The snapshot contains the initialized CUDA worker after synthetic warmup, before real requests.
Service setup runs after restoration. Snapshot preparation logs separate model loading and synthetic warmup durations.
The service-ready metric measures setup after restoration, not the platform's restore duration.

Weights now live in the versioned `bifrost-headroom-models-v1` Modal Volume, mounted read-only by inference.
The CPU-only preparation function downloads pinned revisions once. New model revisions require a new Volume name and snapshots.
The image contains code and dependencies, not weights. The version-checked loader constructs ModernBERT from config before loading the merged checkpoint.
It retains the original checkpoint-completeness checks, precision, attention implementation, and scoring behavior.
Before snapshot capture, the worker waits for the startup canary, synchronizes CUDA, collects unused objects, and releases unused CUDA allocator blocks.
Snapshot preparation records allocated and reserved CUDA bytes. The first measured cleanup released 196 MiB of reserved memory.

Before the Volume change on September 26, snapshot creation plus the first successful request took 63.77 seconds.
After confirmed scale-to-zero, a snapshot-restored request took 42.94 seconds and preserved both sentinel facts.
Platform logs showed approximately 35 seconds between restore start and service readiness, without another model load.
These samples prove restoration works but do not demonstrate lower latency or cost than the non-snapshot deployment.
The configured 30-second scale-down window is not proof of exactly 30 billed idle seconds.
Billing comparisons must include actual startup, snapshot preparation, restoration, and idle resource usage.

With the Volume and allocator cleanup, four cold probes each created a new snapshot: 94.97, 48.48, 42.41, and 74.59 seconds.
Model loading varied from 17.4 to 55.3 seconds. Snapshot reuse performance for this deployment remains unverified.
Two follow-up calls reused the Go client and method, taking 1.47 and 3.29 seconds with identical output to their first calls.
Their server execution took 268 and 387 milliseconds. The remaining round-trip time includes SDK transport and platform work.
The cancellable async SDK path can upload eligible inputs through blob storage because its default inline limit is 8 KiB.
The production compression deadline remains 500 milliseconds, so these results do not establish useful compression under that deadline.
No lower billed cost or representative answer-quality equivalence is claimed from these synthetic probes.

### Private transport and warm GPU comparison, September 26

The private RPC now uses optional lossless zlib/base64 packing in both directions.
Decoded size limits, expiry, explicit remote cancellation, and the 16 KiB tool-input
eligibility gate remain unchanged. Plain requests still receive plain replies.
Packing is used only when smaller; high-entropy bodies can still use Modal's blob path.

Modal's async `Spawn` path defaults to an 8 KiB serialized input/output threshold.
The fixture's 19,532-byte JSON request packs to 501 bytes. On the same T4 worker,
interleaved plain requests took 2.74–3.75 seconds; packed requests and replies took
0.687–0.755 seconds. Request-only packing had taken about 1.53 seconds.
These are transport savings, not additional model compression or billed-token savings.

A subsequent sequential test used 100 warm requests per GPU, after two startup
calls. The same synthetic input contained 18,863 bytes; output contained 14,995
bytes and preserved both sentinel facts. Output was stable within each run.

| Metric | T4 | L4 |
| --- | ---: | ---: |
| Warm end-to-end p50 | 484.65 ms | 358.85 ms |
| Warm end-to-end p95 | 506.52 ms | 380.92 ms |
| Warm end-to-end p99 | 580.05 ms | 408.99 ms |
| Warm maximum | 653.27 ms | 463.78 ms |
| Service p50 | 268.13 ms | 166.90 ms |

These empirical percentiles describe one repetitive fixture from the orb, with
concurrency one, not production traffic or an SLO guarantee. Startup is excluded.
A deployment-cutover run mixed old T4 requests with L4 startup and timed out;
that run was discarded before these measurements. Wait for propagation and
verify accelerator logs before comparing deployments.

An earlier scale-to-zero probe restored a snapshot without another model load:
15.65 seconds end-to-end versus 70.88 seconds for capture plus the first request.
Later redeployments created snapshots again. The worker initialization metric
includes imports, construction, and model loading; it does not isolate Volume I/O.
GPU snapshots do not guarantee faster weight reads. Warm p99 excludes these costs.

L4 is now the sole deployment target; the T4 override has been removed.
The existing 30-second idle window already permits warm
reuse; no minimum replicas or warm pool is added. At listed T4/L4 plus 2 CPU-core
and 4 GiB rates, 30 resource-seconds estimate about $0.00597/$0.00771 respectively,
excluding credits and storage. Actual billing must come from resource metering,
not client wall time. A longer idle window needs request-gap measurements.

The legacy transport benchmark retains its twenty-request safety bound, including two startup calls.
The owner later lifted that limit for bounded pipeline measurements, without changing the $1 Modal limit.

```sh
HEADROOM_MODAL_LIVE=1 HEADROOM_MODAL_SAMPLES=18 GOWORK=off \
  go test -run '^TestLivePrivateModal$' -count=1 -v -timeout 300s
```

Run from `integrations/headroom`. `HEADROOM_MODAL_PROFILE=1` adds four interleaved
plain/packed calls and logs encoding, submission, and result-wait durations.
`HEADROOM_MODAL_REMOTE_PROFILE=1` adds four synchronous SDK calls to profile mode.
The preflight rejects combinations exceeding twenty calls before creating a client.
Small runs report individual times and min/median/max, not p99.
These opt-ins invoke paid GPU work; normal tests skip them.

### Actual gateway-host measurements, September 26

The current Modal deployment uses L4, SDPA, and FP16 for the complete Kompress scorer, including both heads.
MiniLM retains its original precision. No minimum replicas or public endpoint was added.
The owner subsequently approved broad-US placement (`region="us"`), with a 15% resource premium.
`HEADROOM_ATTENTION=eager HEADROOM_PRECISION=default` restores the previous inference configuration at deployment time.

The new client packs short requests even when packing increases their size slightly.
This negotiates compressed replies. A measured MiniLM reply contained 8,197 bytes before packing, exceeding the 8 KiB inline threshold.
Packing reduced it to 5,473 bytes. On one warm container, interleaved embedding calls took 87–107 ms packed versus 215–318 ms plain.
Those ranges exclude the first embedding call. `Spawn`, `Get`, and explicit `Cancel` remain unchanged.

All following client measurements ran on `bifrost-1`, not the orb. They include durable cost-ledger writes.
They measure the Go integration outside the running Bifrost service, not HTTP ingress, semantic-cache lookup, or the upstream provider.

| Measurement | Result |
| --- | --- |
| Initial eager compression service | About 155 ms |
| SDPA compression service, original precision | About 123 ms |
| SDPA + FP16 compression service | About 51–53 ms |
| Packed two-stage FP16 run, 10 warm samples | About 240 ms median, 227–275 ms range |
| Final packed two-stage run, 30 warm samples | 360.19 ms median, 343.31–402.14 ms range |

The final container reported `MODAL_REGION=eu-west`. Bifrost runs in Ashburn.
Transport time varied between runs. Earlier runs did not record compute placement.
Sequential runs do not establish a stable end-to-end improvement.
The provisional 250 ms target is not consistently met. These samples do not establish p99.
Modal charges 1.15× for broad `us` placement and 1.75× for narrow `us-east` placement.
The measurements above preceded the approved broad-US deployment. Narrow `us-east` placement remains disabled.
[Pricing and region options](https://modal.com/docs/guide/region-selection)

Four synthetic compression fixtures covered logs, prose, code, and Unicode text.
FP16 preserved their protected IDs, paths, amounts, flags, and negations.
Three outputs matched SDPA at the original precision exactly. The prose output differed by eight occurrences of “The” and “and.”
This small corpus does not establish general quality equivalence.

Local ARM FP32 MiniLM took 12.1 ms for 14 tokens, 189.7 ms for 256 tokens, and 764.9 ms for a padded four-input batch.
ARM INT8 took 5.0 ms, 81.9 ms, and 337.5 ms respectively.
For the short query, FP32 matched the GPU vector within 1.35e-7 maximum absolute error.
INT8 cosine similarity was only 0.9884. Neither CPU path is enabled in the gateway.

The live gateway still has Headroom disabled. Its 1Password Environment lacks all four required Headroom variables.
The private Modal service is deployed, but the matching gateway package and credentials remain activation prerequisites.

#### Broad-US measurements, September 26

The approved deployment uses `region="us"`, not a narrow region restriction.
Six host runs each started after the container list returned zero containers.
Each run made one cold compression call and ten warm embedding-plus-compression sequences: 126 calls in total.
All runs preserved the same compressed output and protected facts.

| Measurement | Median | Samples |
| --- | --- | --- |
| Cold request with an existing snapshot | 5.50 seconds | 3 |
| Cold request that created a snapshot | 49.99 seconds | 3 |
| All cold requests combined | 48.87 seconds | 6 |
| Logged restore start to service readiness | About 2 seconds | 6 |
| Model loading during snapshot creation | 20.60 seconds | 3 |
| Warm embedding-plus-compression sequence | 289.58 ms | 60 |
| Warm compression call, including transport and admission | 167.02 ms | 60 |
| Warm compression inside the service | 52.62 ms | 60 |

Restored cold requests ranged from 4.90 to 60.65 seconds. The longest request spent most of its time before the restore-start log.
That interval does not identify the exact platform cause. Platform log timestamps have one-second resolution.
The restore measurement excludes scheduling and is not a billed-duration measurement.
Warm sequences had a 225.79 ms median in US East and 325.49 ms in US West, with 30 samples per region.
Modal selected both locations under the 15% broad-US premium. No narrow-region premium applies.
These measurements do not establish p99 or include the running gateway's HTTP and cache stages.

The fixture contains 4,417 `o200k_base` tokens and produces 4,040 tokens, saving 377 tokens (8.54%).
The service's 2,006/1,631 counters are whitespace-word counts, not provider tokens.
The cost analysis uses the actual tokenizer and verifies the saved fixture against the returned SHA-256.
Warm service execution averages 53.42 ms, or about $0.00358 per million input tokens at requested resource rates.
The full warm sequence averages 280.00 ms, or about $0.01874 per million input tokens before startup and idle costs.
A full 30-second idle tail alone costs $0.00887, equivalent to $2.01 per million input tokens for one isolated fixture.
Actual CPU or memory usage above the requested amounts can increase these estimates.

The provider report attributed $0.17133 before credits to this benchmark's app and hourly interval.
That includes 66 compression calls, 60 embedding calls, snapshot creation, startup, and idle resources.
Across 291,522 input tokens, the reported average is $0.588 per million input tokens, or $6.89 per million removed tokens.
This benchmark-specific average includes six container lifecycles and is not a production forecast. Provider reports can update after collection.
The final workspace summary showed $0.84 metered and $0 billed after credits. The $1 provider limit remained unchanged.
The final container list was empty. Local Go race tests passed, and Python tests reported 50 tests with 8 skipped.

The live log database contained zero rows during the read-only usage audit. Row-level security was disabled on that table.
No measured average production token usage or traffic spacing is available from this database.
The benchmark does not justify a monthly production forecast or a GPU upgrade based on current token usage.

For an approved host measurement, run the ARM test binary on the gateway host:

```sh
HEADROOM_PIPELINE_LIVE=1 HEADROOM_PIPELINE_SAMPLES=30 \
  ./headroom.test -test.run '^TestLiveModalPipeline$' -test.v -test.timeout 240s
```

The run makes one startup call and two calls per sample.
`HEADROOM_EMBEDDING_COMPARE=1` interleaves plain and packed embedding requests on the same container.
`HEADROOM_QUALITY_OUTPUT=/private/synthetic-results.json` adds three compression calls and saves synthetic output for comparison.
Credentials must come from the private runtime environment, not command arguments.
The CPU-only `deploy/modal/headroom/benchmark_encoder.py` accepts local pinned ONNX and tokenizer files without network access.

### HTTP versus private SDK transport

The L4 HTTP comparison used ten requests total: one unauthenticated edge probe
(401), one startup request, and four interleaved HTTP/synchronous SDK pairs.
HTTP took 614.86, 414.64, 466.03, and 416.40 ms. Packed synchronous SDK calls took
326.27, 327.53, 327.08, and 330.25 ms. All outputs matched the startup fixture.
This tests HTTP to the same bounded Headroom service, not a different upstream
Headroom SDK pipeline. Four pairs cannot establish p99 or a production SLO.

The temporary HTTP endpoint required Modal proxy authentication before GPU
admission. Its token was deleted and the app redeployed with HTTP disabled.
For an explicitly authorized repeat, use `HEADROOM_HTTP_BENCHMARK=1` at deployment,
then run `TestLiveHTTPComparison` with `HEADROOM_MODAL_LIVE=1`,
`HEADROOM_HTTP_BENCH_URL`, and `HEADROOM_HTTP_TOKEN_FILE`. The credential file must
be mode 0600 and contain the JSON from Modal's proxy-token creation command.
Delete the temporary token and file, and redeploy without the HTTP flag afterward.

The default transport remains packed `Spawn`/`Get` with explicit `Cancel`.
Synchronous `Remote` showed lower latency, but has no durable cancellation
handle. A public endpoint did not improve these paired timings, so no public
endpoint or weaker cancellation path was selected for normal operation.

```sh
# Populate the model Volume before the first deployment of this release.
modal run deploy/modal/headroom/app.py::prepare_weights
modal deploy deploy/modal/headroom/app.py
```

The Go SDK documentation describes [private function calls](https://modal.com/docs/guide/sdk-javascript-go).
Modal documents [invocation and cancellation behavior](https://modal.com/docs/guide/function-invocation-methods) separately.

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
The Overview tab loads metrics through a verified local administrator session. Configuration has a separate tab.
Without that session, the page requires `HEADROOM_METRICS_TOKEN`. Disabled dashboard authentication does not grant metrics access.
The chart shows estimated tool-result tokens per retained attempt. It does not show billed savings or total prompt tokens.
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

## Private Docker Hub runtime image

The Modal deployment imports `docker.io/anshulnoori/headroom` by immutable digest.
Create the Modal Secret `headroom-dockerhub` with `REGISTRY_USERNAME` and a read-only
Docker Hub token in `REGISTRY_PASSWORD`. This Secret is used only for image import;
it is not included in the Headroom function's runtime secrets.

From `deploy/modal/headroom`, publish an authenticated build directly to the registry:

```sh
docker buildx build --platform linux/amd64 \
  --tag docker.io/anshulnoori/headroom:<release> --provenance=false \
  --output type=registry,compression=estargz,force-compression=true,oci-mediatypes=true .
```

Use a BuildKit builder that supports the eStargz exporter. Pin the resulting digest
in `deploy/modal/headroom/app.py`, then run `modal deploy deploy/modal/headroom/app.py`
from the repository root. Do not bake credentials or model weights into the image.
Weights remain in the read-only Modal Volume, and GPU snapshots remain enabled.

On 2026-09-26, the same 11-layer eStargz image was published to Docker Hub and
Google Artifact Registry. Modal recognized both indexes but failed in
`estargz_image::update_ownership_sync` with `No such file or directory`, then used
standard import. Registry migration does not establish a cold-start improvement.
The unpacker failure remains unresolved. Later snapshot reuse measurements appear above.

The L4 still scales to zero: minimum 0, maximum 1, buffer 0, concurrency 1, and
30-second scaledown. Model startup, active execution, and idle time before
scaledown can incur charges. Volume storage and registry charges are separate;
the owner's existing $1 Modal limit is unchanged. No warm pool is configured.
