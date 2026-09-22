# Source findings

The preferred TypeScript SDK plus WASM design does not match the selected Bifrost baseline.
The TypeScript SDK adds an HTTP hop without moving compression out of Headroom's Python service.
The implemented alternative uses the supported native plugin ABI and the same external service directly.

## Sources inspected

| Project | Revision |
| --- | --- |
| Bifrost | `a134a8ce71f8b6c6f5f97b84f192420d6799427b` |
| Headroom | `5ff4ea1ef948563304c9e8f4b9ccff0e2ae3aedd` (0.38.0) |
| LiteLLM | `071cb49d320c9b31fc1d359fca368b8bc5564523` |

Read-only source repositories: https://github.com/maximhq/bifrost, https://github.com/headroomlabs-ai/headroom, https://github.com/BerriAI/litellm.

## Bifrost constraints

`framework/plugins` loads native `.so` plugins. The current loader does not provide the documented historical WASM path.
`core/schemas/plugin_wasm.go` excludes streaming short circuits. Deprecated WASM is not a stable orchestration boundary for CCR.
`RunLLMPreHooks` in `core/bifrost.go` passes typed and passthrough requests through ordered hooks.
`server/plugins.go` places governance and routing before `post_builtin` custom plugins.
Governance denial short-circuits the request. The plugin additionally requires settled identity, access, and limit grants.

Hooks expose primary and fallback attempts. They do not provide every low-level network attempt's usage.
Response mutation or billing derived from estimated compression tokens would misrepresent provider usage.

## Headroom constraints

The TypeScript SDK wraps gateway HTTP contracts. Headroom performs compression inside the Python service.
`gateway_turn.py` and `gateway_extension.py` expose compression, response relay, and redrive obligations.
Gateway turn continuations include process-local suspended coroutines. SQLite CCR persistence does not preserve those continuations.
`gateway_responses.py` uses slot views to preserve opaque Responses items.

Default CCR content hashes use a truncated SHA-256 content hash. They are not authenticated tenant-scoped capabilities.
`ccr/response_handler.py` permits mixed internal and client tool calls to escape unresolved in some paths.
Gateway error handling can return residual internal tools. A client-visible redrive adapter therefore requires additional explicit validation.

Marker-free `/v1/compress` disables CCR store writes when gateway capabilities cannot support retrieval.
The plugin advertises no redrive, relay, or affinity support. It rejects CCR hashes, obligations, and residual retrieval markers.
`--stateless` disables filesystem state but does not prove isolation of TOIN's in-memory learned statistics.
External beacon and local telemetry use separate controls. Both defaults require explicit deployment overrides.

## LiteLLM comparison

The official callback/guardrail integration delegates compression to Headroom rather than reimplementing it.
Some guardrail paths buffer streams. This behavior does not prove native SSE preservation for Bifrost.
This implementation leaves responses untouched and bypasses raw streaming requests instead.

## Cache distinctions

Context compression changes selected prompt content. CCR additionally requires storage, retrieval, and continuation orchestration.
Provider prefix caching belongs to the provider. Explicit cache-control lanes bypass this plugin.
Semantic response caching returns a previous answer and remains disabled.
Compression-result memoization reuses transformations. This plugin does not implement it.

The experimental implementation deliberately omits CCR instead of exposing unsafe handles or replaying client tools.
This omission is a production blocker, not an assertion that a safe sidecar route is impossible.
