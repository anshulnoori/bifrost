# Headroom compression: transport and runtime alternatives

Research date: 2026-09-26. This note records the initial read-only investigation. That phase made no GPU calls or deployments.

The owner subsequently authorized optimization experiments and live gateway activation, and lifted the 20-request limit.
The $1 Modal limit remains unchanged. [Follow-up measurements](README.md#actual-gateway-host-measurements-september-26) supersede this note's unmeasured runtime status.
The optimized Modal service is deployed. Gateway activation still requires four missing 1Password variables and a matching package build.

## Scope and evidence

The supplied Modal service uses Headroom 0.38.0, PyTorch ModernBERT compression, MiniLM embeddings, and a persistent killable CUDA subprocess. Modal uses one L4 container, concurrency 1, minimum 0, maximum 1, buffer 0, a 30-second idle window, GPU snapshots, and read-only weights. The intended private Go client uses `Spawn` / `Get` with explicit `Cancel`. Bounded zlib/base64 packing avoids asynchronous blobs on repetitive fixtures.

The local implementation was inspected in `/home/user/workspace/monorepo/src/ai/bifrost`; the live host was inspected separately. Historical orb timings of approximately 170 ms service execution and 350–700 ms total are not Bifrost-host timings. They cannot establish the current bottleneck. Service execution also includes validation, IPC, tokenization, and output handling: it is not a pure GPU-kernel measurement.

The Headroom 0.38.0 source archive passed its published SHA-256 check: `63800dfc037fcb38dfdb2d0582f672218c489048546a76de70fb9afde9312f59`. The release tag resolves to [this commit](https://github.com/headroomlabs-ai/headroom/commit/94206e265203acfd72a3b939e9a964e29175ad50). Modal Go `v0.10.1` resolves to [this commit](https://github.com/modal-labs/modal-client/commit/3b4ed40f22cf3898a210139676bc1b1fcf1a2084). [S1–S5]

## Verified deployment state changes the priority

These observations were checked through SSH to `bifrost-1` in [this thread](https://ampcode.com/threads/T-01a0ccf1-cb6b-749a-b105-fd89dba2aefb). Configuration inspection projected selected non-secret fields only; process inspection printed credential variable names, never values.

1. `bifrost-1` is an OCI `VM.Standard.A1.Flex` aarch64 host in Ashburn (`iad`).
2. The live service still uses the old HTTP plugin configuration. Outer `enabled` is true, but `config.enabled=false`, the endpoint is empty, and the timeout is 500 ms. `min_text_bytes` is 4096. The process has no `HEADROOM` or `MODAL` variables.
3. Semantic cache is direct-only, with an empty provider and dimension 1. This configuration does not establish an operational MiniLM semantic cache.
4. The intended gateway deployment and runtime credential installation are not active. The 1Password import remains a human step; its current store contents were not inspected. No active Bifrost-to-Modal inference latency exists to report.
5. Host HTTPS probes to the `api.modal.com` root showed approximately 3 ms fresh TCP time with cached DNS. TLS completion was approximately 25 ms cumulative. A full HTTP/2 response took 145–185 ms. These are not gRPC, Function, or inference measurements.
6. The intended source path is direct cache → Modal semantic embedding → semantic cache decision → Modal Headroom compression → provider.
7. The intended local durable cost admission performs file `Sync`, rename, and directory `Sync` once per logical embedding/compression invocation, not once per SDK transport RPC. This is confirmed in [cost.go](cost.go#L25-L105), [bridge.go](bridge.go#L178-L205), and [metrics.go](metrics.go#L123-L146). Prior orb benchmarks had an empty `CostLedgerPath`, so they excluded this work.

The observed host process started on September 25 and runs `/nix/store/y4ja82s13kmc4n7qsawdl7xdmr3lnphb-package/bin/bifrost-http`. The inspected file was `/var/lib/bifrost/config.json`. A local `/health` request returned 200 in 37.8 ms; this is a health-route measurement, not a proxy or compression request. Intended private SDK settings, embedding dimension 384, and the 16 KiB gate are in [module.nix](../../deploy/nixos/module.nix#L137-L173), not the active host configuration. The [disabled pre-hook gate](main.go#L90-L102) explains why no Modal compression call currently occurs.

Ashburn is already near Modal's documented default Virginia routing region. That weakens the case for changing routing regions. It does not establish the GPU container's location or its allocation latency. The HTTP-root response time cannot be multiplied by the number of SDK calls to predict inference latency. It includes endpoint-specific work, and reused gRPC connections have a different lifecycle. [S3–S5, S15]

## Count remote stages and durable admissions before changing providers

On a direct-cache hit, the intended path needs no embedding or compression call. On a semantic-cache hit after a direct miss, it needs embedding but no compression. On a full miss with eligible content, it needs two serial Modal calls before the provider request. These counts describe logical invocations, not every network operation.

For each `Spawn` / `Get` invocation, the inspected Go SDK performs a submission RPC and at least one output RPC. Two serial stages therefore imply at least four Function RPCs on the normal completed path. Function lookup, connection establishment, authentication, blob transfers, retries, and repeated long polls can add work. If both stages share a warm container, the second stage need not cause another GPU cold start. That deployment relationship remains unverified. [S3–S5]

An eligible full miss needs separate time spans for local admission, embedding, semantic lookup, compression, and provider admission. Source inspection confirms durable admission once per logical remote stage. The intended embedding deadline is two seconds; compression has a separate 500 ms deadline. Neither is an end-to-end two-stage latency measurement. Ledger work runs before the per-stage RPC timeout is created, so it must be timed explicitly.

**The first architecture candidate is local MiniLM on the existing ARM host.** It can remove the embedding network stage while keeping the cache decision local. A warm, bounded CPU worker can preserve cancellation isolation. FP32 ONNX is the parity baseline. An ARM64 INT8 artifact is a later experiment, not an assumed speedup. Sentence Transformers documents an `arm64` quantization configuration. CPU allocation, current load, supported kernels, and model latency remain unknown. [S7, S26]

**Exact embedding reuse is lower-risk than speculative compression.** A tenant-scoped embedding cache can skip repeated embedding calls for identical input and model configuration. It must preserve tokenizer, normalization, truncation, and model identity. Embedding-space or dimension changes require compatible cache invalidation. The live dimension-1 configuration must not silently reuse entries as MiniLM vectors.

**Combining both remote stages is not automatically beneficial.** A single call that computes embeddings and compression before the local semantic lookup wastes compression on semantic hits. It can increase paid work. Moving the semantic lookup into the remote call avoids that speculation, but moves cache data and authorization into Modal. A stateful two-message session still has two dependent network exchanges. None is a free replacement for local embedding.

**Durability is part of the cost-control contract, not removable overhead.** Measure file sync, rename, directory sync, lock contention, and queueing separately. Do not replace them with asynchronous writes to improve a benchmark. A grouped reservation is only a future design option if it durably reserves the full bounded multi-stage cost before transmission. It also needs crash recovery, idempotency, unused-reservation handling, and concurrency proofs. No ledger change is recommended without that investigation.

## Ranked experiments, not promised speedups

The rank balances evidence, latency potential, implementation effort, and preservation of cancellation.
The initial research included no GPU experiments. The owner later lifted the 20-request cap but retained the $1 Modal limit.
New paid infrastructure and region-selection premiums still require approval.

| Rank | Alternative and reason | Required measurements | Falsification / rejection criterion |
|---|---|---|---|
| 1 | Count serial stages and durable admission. Preserve `Spawn` / `Get` / `Cancel`, reuse client and hydrated Function, retain bounded packing. Consider binary CBOR packing. [S3–S5, parent observations] | Direct/semantic miss rates, lookup and RPC count, connection reuse, ledger sync/lock time, blob rate, cancel-to-idle time. | Existing reuse is complete and transport/ledger overhead is immaterial. Never accept weaker durable cost admission as a speedup. |
| 2 | Local ARM MiniLM or exact embedding reuse removes one remote stage without speculative compression. [S7, S26] | ARM CPU/RSS/queueing, embedding and semantic-decision parity, cache hit rate, full-miss p95, saved admissions and RPCs. | Local work exceeds eliminated remote time, cache identity is incomplete, or semantic ranking changes outside the quality limit. |
| 3 | Small Headroom fork: eager attention → SDPA. Then BF16/FP16 separately. Also measure batch D2H and vectorized word reduction. GPU chunk batching already exists. [S2, S10–S12] | Actual dtype/kernels, synchronized encoder/head time, tokenizer/selection spans, shape padding, warm/restored p95, selected-word parity. | No material total improvement, unsupported fused masks, unacceptable word changes, or worse restoration tails. |
| 4 | Local hybrid: deterministic Rust structural compression and size-based passthrough at Bifrost. Local ONNX MiniLM/Kompress only where CPU budget permits. [S6–S8] | Content-type distribution, real host CPU queueing/RSS, token savings, end-to-end time, answer-evidence retention. | CPU contention exceeds eliminated network time, most inputs require ML, or structural rules remove required evidence. |
| 5 | Compile the actual encoder or `get_scores` callable, with bounded shape buckets and snapshot warmup. `reduce-overhead` targets Python dispatch without a rewrite. [S2, S13, S14] | Compile/recompile counts, synchronized steady-state time, first request after restore, memory, unseen shapes, worker restart after cancellation. | Compile cost escapes startup, CUDA graphs do not apply, new shapes regress p95, or snapshot/worker recovery fails. |
| 6 | Authenticated Modal Server HTTP path, or Web Function HTTP path. This can remove the asynchronous Function transport. Servers specifically target low-latency HTTP. [S15–S17] | Same payload and model, actual host HTTP timings, cold 503 recovery, disconnect behavior, cancel acknowledgement, work-stop latency. | Gains disappear after request tracking/retries/cancellation, or cancellation cannot reach a busy worker. |
| 7 | ONNX Runtime CUDA first, then TensorRT with fixed profile ranges. Existing export includes both Kompress heads. A native service can reuse that graph. [S9, S18, S19] | Graph partitioning, kernel/provider assignments, transfers, engine build/cache time, scores and selected words, dynamic-shape tails. | CPU fallback, repeated engine builds, copy overhead, numerical drift, or slower total time than optimized PyTorch. |
| 8 | Async precompression of immutable tool results before the next LLM request. Keep a synchronous fallback. | Ready-at-use fraction, canceled/unused work, cache keys, request p95, cache-prefix stability, stale-result rejection. | Work rarely finishes before use, speculative work increases cost, or query changes invalidate most results. |
| 9 | Geography changes are low priority: Ashburn is already near default Virginia routing. Actual GPU placement remains unknown. Non-default Function routing excludes Spawn. [S15, parent observations] | Actual container region, cross-region contribution, cold allocation tails, cost per completed request. | No remote compute-location penalty, paid region selection violates the cost limit, or restricted capacity increases tails. |
| 10 | More radical runtime/model/provider changes: reuse Headroom Rust core, LLMLingua-2-small, Runpod queue endpoints, or Cloud Run L4. [S6, S20–S22] | Same full quality corpus and workload distribution, cancellation, cold/warm time, operational burden, total cost. | No material advantage over optimized current architecture, quality loss, weak cancellation, or additional paid infrastructure. |

## Modal transport: useful options and hard constraints

1. **Current Go `Spawn` uses the control plane.** It calls `createControlPlaneInvocation`, then returns a `FunctionCall`. `Get` long-polls `FunctionGetOutputs`. There is no fixed polling sleep in the inspected loop. A shorter poll interval is not an established latency improvement. [S3–S5]
2. **`Remote` has a different path, but no equivalent public cancellation handle.** It selects the input plane when Function metadata supplies `InputPlaneUrl`. Otherwise, it uses the control plane. Its inspected error path returns without explicit `FunctionCallCancel`. Context cancellation stops the client wait, but that is not proof that remote GPU work stops. Replacing `Spawn` with `Remote` needs a separate cancellation design. [S3–S5]
3. **The 8 KiB threshold is real, but it is not universal.** Go defaults to 8 KiB for asynchronous objects and 2 MiB for general objects. Function metadata can override both. The threshold applies after CBOR serialization. Input and output behavior require separate observations. [S3, S5]
4. **Binary packing is a supported serialization candidate, not a guaranteed improvement.** The client serializes arguments as CBOR. A byte string can carry compressed data without base64 expansion. Both language boundaries still need interoperability tests. Packing metadata and CBOR envelope bytes count toward thresholds. Incompressible input can still require blobs. Bounded decompression remains mandatory. [S3]
5. **Compute placement and routing placement are separate.** Default Function routing passes through Virginia (`us-east`). Functions offer routing regions `us-east`, `us-west`, `eu-west`, and `ap-south`. The current guide permits non-`us-east` routing only for `.remote()`, `.map()`, and HTTP Web Functions, not `.spawn()`. A routing-region change requires a new Function. Objects above 2 MiB still use object storage in `us-east`. [S15]
6. **Region pinning is not free.** The live page fetched for this note lists 1.15× for broad compute regions and 1.75× for narrow regions. Search-index excerpts showed an older broad multiplier, so pricing needs a fresh check before a decision. Region restriction also reduces the available scheduling pool. It cannot be an unapproved implementation step under the no-extra-paid-work constraint. [S15]
7. **Web Functions support Python handlers, ASGI/WSGI, arbitrary HTTP servers, and WebSockets.** A native Rust/C++ server can run behind `web_server`. WebSocket connections count as Function inputs and can keep a container active. They can conflict with the existing idle-cost goal. Proxy tokens protect HTTP access. HTTP disconnect must not substitute for an explicit work-stop contract. [S16, S17]
8. **Modal Servers are a distinct transport alternative.** Servers use a stateless reverse proxy rather than the Function input system. They require authentication by default. They do not queue requests during scale-from-zero: requests return 503 and trigger startup. Concurrency targets are soft, so the application needs its own admission control. Servers lack built-in Function retries and configurable Modal request timeouts. The application must supply deadlines and cancellation. The guide lists sticky routing through `Modal-Session-ID`. SDK availability in the deployed Python environment remains unverified. [S17]
9. **Tunnels offer direct TLS/TCP connections.** Modal describes them as its lowest-latency direct path to an active container. They have random public addresses, not automatic application authorization. A singleton supervisor could publish the current address, authenticate requests, and own worker cancellation. Discovery, container lifecycle, replay protection, and restarts become application responsibilities. An always-running tunnel owner also conflicts with the current scale-to-zero policy. [S23]
10. **Sandboxes offer authenticated HTTP/WebSocket Connect Tokens and direct port access.** They are a possible native-runtime host, not a drop-in Function replacement. Explicit lifecycle management and per-request cancellation remain necessary. A Sandbox termination cancels the whole environment, not an isolated compression request. [S24]

The lowest-risk transport improvement preserves the existing call handle and subprocess boundary. Cancellation cleanup needs its own bounded context after the request context expires. A race before `Spawn` returns can leave the caller without a handle. Server-side deadlines or request IDs must cover that case. These are design requirements, not observed defects in the parent implementation.

For HTTP designs, an independent supervisor must accept cancel requests while the model runs. An inference semaphore must not block the cancel route. Each request needs an unguessable ID, deadline, ownership check, and generation ID for the worker. A cancel acknowledgement is insufficient: validation must show that work stops and capacity becomes available. Worker termination and recreation provide a stronger boundary than abandoning a thread or coroutine.

## What Headroom actually computes

The following facts describe upstream 0.38.0, not uninspected deployment overrides. [S2]

1. The model contains a ModernBERT encoder, a two-class linear token head, and a two-layer Conv1d span head. The span layers use GELU and sigmoid. `get_scores` returns `token_probability × (0.5 + 0.5 × span_score)`.
2. The constructor explicitly selects `attn_implementation="eager"`. Loading moves the model to its device and calls `eval()`. The loader does not explicitly select half precision. Actual runtime dtype remains unknown.
3. Both scoring methods use `torch.no_grad()`. A switch to `inference_mode()` is a separate, measurable optimization, not a claim that gradients currently run.
4. Default chunks contain 350 words. Tokenization uses pre-tokenized words, truncation at 512 tokens, and padding. This is not an 8192-token ModernBERT forward pass.
5. On CUDA/MPS, `compress()` already delegates to `compress_batch()`. A proposal to add batching must distinguish existing intra-request batching from cross-request batching.
6. Each batch transfers input tensors to the device, runs the encoder and both heads, and copies each result row to CPU. Python then reduces subword scores to maximum word scores. It sorts for target-ratio selection or applies a threshold, restores must-keep words, and reconstructs the output.
7. The Python must-keep override protects words such as numbers, error names, paths, and flags. Rewrites must retain that policy, independent of score agreement.
8. Sequential PyTorch `get_keep_mask()` uses a borderline/span-boost rule. Batched scoring and ONNX use a combined score threshold. They are not identical contracts when `target_ratio=None`.

**Source code identifies work, not its measured dominance.** The encoder performs attention and feed-forward tensor operations. These run through native PyTorch kernels, not a Python implementation of matrix multiplication. Transformers v5.17.0 preserves explicit eager selection through `AutoModel.from_config`; eager materializes attention products and FP32 softmax. ModernBERT also declares SDPA support. Tokenization, IPC, packing, tensor copies, scalar conversion, sorting, MiniLM inference, and queueing can still dominate short requests. [S2, S10]

**The upstream batched `inference_ms` counter is not a GPU duration.** It stops immediately after `get_scores`, before the later `.cpu()` calls. CUDA execution is asynchronous, so that counter can exclude GPU completion. Sequential timing includes the result copy. The canary also returns without an explicit completion barrier. A profiler or CUDA events with synchronization must establish GPU time. This does not invalidate an external timer that surrounds the completed service call. [S2, S25]

MiniLM is a separate model pass, not part of the Kompress span head. Upstream includes both a SentenceTransformer embedder and an ONNX embedder. The ONNX implementation tokenizes a batch, runs the encoder, performs masked mean pooling, then normalizes the embeddings. [S7] The local [encoder.py](../../deploy/modal/headroom/encoder.py) uses `all-MiniLM-L6-v2`, masked mean pooling, L2 normalization, and 384-dimensional vectors on its supplied device. It tokenizes up to 257 tokens to detect and reject inputs exceeding 256, rather than silently treating identical prefixes as equivalent. A local ONNX replacement must preserve that rejection behavior.

## Replacing Python: plausible benefits and limits

**A language-only rewrite cannot remove model arithmetic or network distance.** It can reduce orchestration, serialization, scalar handling, startup imports, and memory. Compilation can reduce Python dispatch while preserving the current service design. A native ONNX/TensorRT runtime changes the execution engine as well as the language, so its gains cannot be attributed only to Rust or C++. [S13, S18, S19]

**Headroom already includes a reusable Rust implementation.** `headroom-core::transforms::kompress` uses Rust tokenizers and ONNX Runtime. Its graph contract is integer `input_ids`/`attention_mask` in and floating-point `final_scores` out. `from_files` supports explicit local artifacts. No fresh model architecture implementation is necessary. [S6]

That implementation is not a drop-in replacement for the supplied PyTorch service:

1. Its inspected session builder does not explicitly configure a CUDA execution provider. Its chunk loop runs sequentially. GPU provider registration and batching require work. [S6]
2. Its minimum is 10 words, whereas Python configuration defaults to 64. Its inspected selector lacks the Python must-keep override. CCR markers intentionally differ. [S2, S6]
3. Its parity test compares ONNX fixtures and skips when models or fixtures are absent. That test does not prove parity with this deployment or full cancellation behavior. [S8]
4. Its cache resolver searches available snapshots. An integration needs explicit artifact revisions and hashes, not whichever snapshot is present. [S6]

For a native fork, the smallest useful boundary is the scoring engine plus its tokenizer contract. The existing supervisor can retain deadlines, cancellation, validation, and output reconstruction. A native subprocess preserves killability better than embedding a long-running FFI call directly inside the Bifrost process.

## Runtime and model choices

### Same L4, optimized PyTorch

SDPA is the first inference experiment because the eager setting is explicit. Fused kernel selection depends on dtype, shape, mask, device, and library version. A configured SDPA model is not proof that FlashAttention runs. ModernBERT alternates local and global attention. Mask parity needs tests on both paths. Reduced precision can change score rankings and threshold decisions. [S10–S12]

Version-specific inspection used Transformers v5.17.0, matching the local lockfile. Local attention and padded inputs can require explicit masks; `local_attention=128` uses a half-window of 64. Compilation and CUDA stream capture count as tracing and can prevent mask elision, changing kernel selection. This is a performance qualification, not proof that compilation fails. Version 5.17 already computes rotary embeddings once per distinct layer type, so that optimization is not missing. Constructor dtype can come from model configuration: absence of a worker override does not prove FP32. Inspect actual tensors before comparing SDPA at the same dtype, reduced precision across both encoder and heads, and finally compilation. [S10]

BF16 and FP16 are separate experiments. Threshold-adjacent words and near-tied rankings need special coverage. TF32 is another optional FP32-matmul tradeoff, but runtime defaults require inspection before a recommendation. Generic framework throughput benchmarks do not predict batch-1 request latency. [S12, S26]

Compilation must target the callable that actually runs. Kompress defines `get_scores` and `get_keep_mask`, rather than a conventional top-level `forward`. Compiling only an unused wrapper gives no benefit. Compiling the encoder is a small starting point. A combined scoring callable can include the two heads. [S2, S13]

Shape buckets can reduce recompilation and padding. Any bucket change must preserve chunk boundaries, word mapping, and truncation behavior. Model training and chunk size are coupled. A larger chunk is a semantic change, not only a faster batch. [S2]

Modal recommends warm forward passes before a GPU snapshot. It also documents snapshot/compile failures and a possible `TORCHINDUCTOR_COMPILE_THREADS=1` workaround. GPU snapshots do not necessarily accelerate weight loading, and read-only weights cannot hold runtime-generated engine caches. Engine/compile caches need an image artifact or a separate writable location. Compatibility must include child-process restart after cancellation. [S14]

### ONNX Runtime and TensorRT

Headroom provides an export script for the complete scoring graph, not only the base encoder. It exports dynamic batch/sequence axes and `final_scores`, with default opset 17. Its score formula matches `get_scores`. The default ONNX candidates include weight-only INT8 and FP32. [S2, S9]

The built-in Python ONNX selector supports CPU and CoreML, not CUDA/TensorRT. A GPU ONNX experiment requires an explicit provider change. TensorRT requires supported operators and shape profiles. Unsupported sections can fall back to another provider. Engine caches depend on model, runtime, precision, profiles, and GPU compatibility. Unexpected shapes can trigger rebuilding. [S2, S18]

Start from the FP32 scoring graph for parity, then evaluate FP16. The existing `MatMulNBits` weight-only artifact is not automatically the best TensorRT input. ORT I/O binding can prevent unwanted device copies. This workload ultimately needs CPU-selected words, so at least the final decision data must return to CPU. [S2, S18, S19]

ORT exposes `RunOptions::SetTerminate()`. Its documentation promises termination of matching `Run` calls, but does not establish a bound on interruption of a running GPU kernel. A killable worker remains necessary until measured cancellation meets the contract. [S27]

### Local CPU, hybrid routing, and asynchronous work

Local structural compression removes remote latency for suitable inputs. Headroom already has Rust structural transforms and a content router. A conservative policy can preserve short or dense inputs and send only eligible prose to ML. Local CPU inference is useful only if compute plus queueing costs less than the eliminated remote path. The parent identifies the host as ARM64. Its CPU allocation, load, and available memory remain unknown. [S6, S28, parent observations]

ONNX MiniLM offers a smaller deployment dependency than PyTorch. Sentence Transformers also documents ONNX INT8 and OpenVINO variants. Outside its wrapper, callers must preserve pooling, normalization, tokenizer, and truncation. Embedding parity must cover downstream ranking and deduplication decisions, not only vector similarity. [S7, S26]

Precompression can move work outside the next request's critical path. Immutable tool-output bytes are the safest unit. A reusable key needs tenant, content hash, model/tokenizer revision, policy, and target ratio. Query-dependent stages also need query/context identity. Raw chunk scores can be more reusable than final selected words. Cache scope, expiry, deletion, and access control must match the original content.

Async work does not eliminate compute. It can increase unused work and retain sensitive data longer. The current no-extra-paid-work constraint excludes speculative remote calls until their cost is approved. A bounded local queue and synchronous passthrough provide a lower-risk design hypothesis.

### Other models and providers

LLMLingua-2-small is a concrete BERT-level token-classification alternative. Its published example uses `microsoft/llmlingua-2-bert-base-multilingual-cased-meetingbank`. The repository's speed claims compare LLMLingua variants, not this L4 Kompress deployment. A model swap needs new quality evaluation for code, logs, identifiers, negation, and retrieval. Distillation of Kompress to a smaller encoder is another hypothesis, but training exceeds this research scope. [S20]

Runpod offers asynchronous jobs, synchronous jobs, and `/cancel` for queued/running jobs. That is a closer cancellation API match than bare HTTP, but documentation does not prove a latency or hard-stop advantage. Cloud Run supports L4 and scale-to-zero, but requires at least four CPUs and 16 GiB memory. GPU billing covers the instance lifetime. Neither provider is demonstrably better without new measurements and paid infrastructure. [S21, S22]

Co-location on already provisioned hardware is the clearest non-Modal way to eliminate the public network hop. It trades that hop for local resource contention and operational responsibility. Moving Bifrost itself to GPU infrastructure broadens the failure domain and can increase idle GPU cost. It is not justified by the old orb timings.

## Measurement and acceptance contract

These are future measurements for the owner, not actions performed by this research task.

1. Record the exact package versions, model/tokenizer revisions, dtypes, kernel selections, and active compression policy.
2. Separate cold, snapshot-restored, warm-idle, and back-to-back calls. Record actual compute and routing regions.
3. Partition payloads by text type, byte size, token length, chunk count, and compressibility. Include incompressible data near the serialized blob threshold.
4. Record durable admission, lock waits, packing, lookup, submission, queueing, restore, subprocess IPC, tokenization, embeddings, synchronized scoring, selection, and decoding.
5. Compare paired runs against the same baseline. Predeclare a material p95 improvement and a permitted quality tolerance before GPU experiments.
6. Validate protected facts exactly: IDs, paths, numbers, flags, negation, permission boundaries, error lines, and tool-call/result pairing.
7. Compare scores, selected-word sets, reconstructed output, saved tokens, and exact original retrieval. Compression ratio alone is not a quality metric.
8. Exercise cancellation before submission, during queueing, during inference, during serialization, and just before completion.
9. Measure cancellation acknowledgement separately from worker exit, GPU quiescence, next-request admission, and restart latency.
10. Preserve bounded decoding, authentication, read-only pinned weights, tenant-scoped caches, offline loading, and content-free operational logs.

The intended path is not live, so no deployed inference bottleneck or speedup is established. The first priorities are serial RPC/admission accounting and local ARM embedding. Eager-to-SDPA remains the strongest small inference experiment. A wholesale Python rewrite or provider move has less supporting evidence. Local/hybrid work or a supervised native HTTP scorer remains conditional on quality, cancellation, and cost.

## Primary sources

S1. Headroom 0.38.0 release metadata and source-archive hash: https://pypi.org/pypi/headroom-ai/0.38.0/json

S2. Headroom 0.38.0 Python Kompress implementation, especially lines 521–610, 883–947, 1248–1274, 1599–1611, 1713–1758, and 2051–2155: https://github.com/headroomlabs-ai/headroom/blob/94206e265203acfd72a3b939e9a964e29175ad50/headroom/transforms/kompress_compressor.py

S3. Modal Go v0.10.1 Function serialization, thresholds, Remote, and Spawn: https://github.com/modal-labs/modal-client/blob/3b4ed40f22cf3898a210139676bc1b1fcf1a2084/go/function.go

S4. Modal Go v0.10.1 FunctionCall Get/Cancel: https://github.com/modal-labs/modal-client/blob/3b4ed40f22cf3898a210139676bc1b1fcf1a2084/go/function_call.go

S5. Modal Go v0.10.1 control-plane/input-plane invocation and polling: https://github.com/modal-labs/modal-client/blob/3b4ed40f22cf3898a210139676bc1b1fcf1a2084/go/invocation.go

S6. Headroom 0.38.0 Rust Kompress implementation: https://github.com/headroomlabs-ai/headroom/blob/94206e265203acfd72a3b939e9a964e29175ad50/crates/headroom-core/src/transforms/kompress.rs

S7. Headroom 0.38.0 MiniLM implementations: https://github.com/headroomlabs-ai/headroom/blob/94206e265203acfd72a3b939e9a964e29175ad50/headroom/memory/adapters/embedders.py

S8. Model-gated Rust parity test: https://github.com/headroomlabs-ai/headroom/blob/94206e265203acfd72a3b939e9a964e29175ad50/crates/headroom-core/tests/kompress_parity.rs

S9. Versioned dual-head ONNX export: https://github.com/headroomlabs-ai/headroom/blob/94206e265203acfd72a3b939e9a964e29175ad50/scripts/export_kompress_v2_onnx.py

S10. Transformers v5.17.0 ModernBERT and framework source, matching the local lockfile (not proof of the running image's installed version): [ModernBERT](https://github.com/huggingface/transformers/blob/v5.17.0/src/transformers/models/modernbert/modeling_modernbert.py), [constructor selection](https://github.com/huggingface/transformers/blob/v5.17.0/src/transformers/modeling_utils.py#L1398-L1436), [masking](https://github.com/huggingface/transformers/blob/v5.17.0/src/transformers/masking_utils.py#L309-L337), [tracing](https://github.com/huggingface/transformers/blob/v5.17.0/src/transformers/utils/import_utils.py#L1806-L1818), and [SDPA adapter](https://github.com/huggingface/transformers/blob/v5.17.0/src/transformers/integrations/sdpa_attention.py#L87-L185).

S11. ModernBERT SDPA and padding-free guidance: https://huggingface.co/docs/transformers/model_doc/modernbert

S12. PyTorch 2.12 SDPA kernel selection and numerical differences: https://docs.pytorch.org/docs/2.12/generated/torch.nn.functional.scaled_dot_product_attention.html

S13. PyTorch 2.12 compilation modes, graph constraints, and recompilation: https://docs.pytorch.org/docs/2.12/generated/torch.compile.html

S14. Modal GPU snapshots, warmup, compilation, and storage limitations: https://modal.com/docs/guide/memory-snapshot

S15. Modal region/routing restrictions and pricing, live-refetched: https://modal.com/docs/guide/region-selection

S16. Modal Web Functions, arbitrary servers, WebSockets, authentication: https://modal.com/docs/guide/webhooks

S17. Modal Servers, authentication, scale-from-zero, routing, and operational differences: https://modal.com/docs/guide/servers

S18. ONNX Runtime TensorRT provider, shape profiles, fallback, and caches: https://onnxruntime.ai/docs/execution-providers/TensorRT-ExecutionProvider.html

S19. ONNX Runtime I/O binding and implicit device copies: https://onnxruntime.ai/docs/performance/tune-performance/iobinding.html

S20. Microsoft LLMLingua-2 and small-model example: https://github.com/microsoft/LLMLingua

S21. Runpod job APIs, cancellation, and execution policies: https://docs.runpod.io/serverless/endpoints/send-requests

S22. Cloud Run GPU requirements and billing: https://cloud.google.com/run/docs/configuring/services/gpu

S23. Modal tunnels, direct connections, public access, and pricing: https://modal.com/docs/guide/tunnels

S24. Modal Sandbox Connect Tokens and networking: https://modal.com/docs/guide/sandbox-networking

S25. PyTorch CUDA asynchronous execution and timing guidance: https://docs.pytorch.org/docs/2.12/notes/cuda.html#asynchronous-execution

S26. Sentence Transformers precision, ONNX/OpenVINO, pooling, and workload-sensitive benchmarks: https://sbert.net/docs/sentence_transformer/usage/efficiency.html

S27. ONNX Runtime termination API: https://onnxruntime.ai/docs/api/c/struct_ort_1_1_run_options.html

S28. Versioned content router and native compression integration: https://github.com/headroomlabs-ai/headroom/blob/94206e265203acfd72a3b939e9a964e29175ad50/headroom/transforms/content_router.py

S29. Modal HTTP timeout/result-URL behavior, distinct from cancellation: https://modal.com/docs/guide/webhook-timeouts
