## ✨ Features

- **Anthropic Between-Tools Thinking** - `reasoning.type: "between_tools"` on chat and Responses requests, and `thinking: {"type": "between_tools"}` on the Anthropic drop-in route, are forwarded to Anthropic, Bedrock and Vertex with the caller's effort passed independently. Models without it get `disabled` or no thinking field, so a fallback to an older model never fails. The datasheet `supports_between_tools_thinking` field can override this (#7665)
- **Tool Search for GPT-5.4 and Later** - `defer_loading` on function and MCP tools now reaches OpenAI, Azure, Bedrock and Bedrock Mantle for gpt-5.4, gpt-5.5, gpt-5.6 and gpt-6 models, so the model can search deferred tools instead of loading every tool eagerly. Other OpenAI-compatible backends still have it stripped. The datasheet `supports_tool_search` field can override this (#7534)
- **Skipped Routing Fallbacks Are Logged** - A rule fallback that names no known provider is now reported in the request's routing log with the rule name and the configured entry, instead of being skipped silently (#7546)

## 🐞 Fixed

- **Anthropic Bedrock Request Metadata** - Cover Anthropic and PydanticAI request metadata passthrough to Bedrock [@wangrat](https://github.com/wangrat)
- **Kimi and DeepSeek with Claude Code** - Tool-schema regex patterns are rewritten (`\0` to `\x00`, lookaround assertions stripped) for Moonshot and DeepSeek models only. kimi-k3 on Bedrock no longer returns an empty stream, and every other model gets byte-identical schemas (#7430)
- **Anthropic Billing Header Leak** - Claude Code's `x-anthropic-billing-header` system block is removed at Messages ingress and restored only for Anthropic-family attempts, including fallbacks and alias targets, so it no longer pollutes GPT or Gemini prompts (#7431)
- **MCP Egress Proxy** - MCP HTTP/SSE connections honor `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` again (broken since core v1.8.5). Link-local and unspecified destinations are refused before the proxy is dialed (#7437)
- **OpenAI `computer` Tool** - The bare `{"type":"computer"}` tool is no longer rewritten to `computer_use_preview`, which fixes computer use on GPT-6 Astra and GPT-5.6 (#7426) (thanks [@abhishekgahlot2](https://github.com/abhishekgahlot2)!)
- **Streaming Memory Leaks** - The request context is cancelled on every stream exit path, not only on write errors, which stops leaked disconnect watchers from pinning request contexts. `StripEmptyThinkingBlocks` rewrites the body once instead of once per block, and Anthropic beta-header gating no longer decodes the full request body (#7406)
- **Bedrock cachePoint Leak** - Bedrock `cachePoint` markers are stripped copy-on-write for non-Bedrock providers and kept for Bedrock fallbacks. The compat plugin no longer mutates the shared request. Nova reasoning signatures are stripped on the wire only (#7182)
- **Bedrock Empty JSON Keys** - Tool results containing an empty-string object key (such as Cursor's `list_directory`) are sent as text, so Converse no longer rejects them (#7396)
- **Bedrock cache_control on String Content** - InvokeModel keeps every `cache_control` when any message's content is a plain string (#7360) (thanks [@basil-k-aji-dev](https://github.com/basil-k-aji-dev)!)
- **Web Search Source Names** - Responses web search API sources keep their `name` and no longer emit an empty `url` (#7358) (thanks [@g-yixuan](https://github.com/g-yixuan)!)
- **Grok 4.7 xhigh Reasoning** - `xhigh` reasoning effort is no longer downgraded to `high` (#7403) (thanks [@nettee](https://github.com/nettee)!)
- **Allow-All Provider Access** - Virtual keys that allow every provider now list models from, and route to, every configured provider. The governance routing log names providers excluded for having no weight (#7375)
- **OpenAI Chat Stream Framing** - Bundled raw finish and usage frames on the OpenAI chat stream passthrough each get their own `data:` prefix (#7440)
- **Responses Deep Copy** - `DeepCopyResponsesMessage` now deep copies cache controls, provider-native parts, computer/MCP/code-interpreter tool fields and annotations, so copies no longer share pointers with the original (#7422)
- **Request Preparation Performance** - Responses requests are decoded once instead of several times, and the compat plugin clones only the fields it writes (#7412, #7097) (thanks [@G-XD](https://github.com/G-XD)!)
- **Virtual Key PUT Round-Trip** - PUT `/api/governance/virtual-keys/{vk_id}` no longer drops provider-config key associations when the request body omits `key_ids`. The GET response exposes `allow_all_keys` and `keys` but not `key_ids`, so the standard GET -> edit -> PUT round-trip silently flipped AllowAllKeys to false and detached every key, and inference through the virtual key then failed with "no keys found for provider". An omitted `key_ids` list now leaves the existing associations untouched; an explicit list (including `[]`) still replaces them (#7347) (thanks [@xiechimon](https://github.com/xiechimon)!)
- **Routing Fallbacks Dropped After Restart** - Legacy `provider/model` fallback strings are re-parsed at route time, so a custom provider registered after routing rules were decoded at boot is no longer skipped while the API still listed the fallback. Object-form fallbacks naming an unregistered or blank provider are now rejected on create and update (#7543, #7544, #7545)
- **Bedrock Thinking Tokens** - Requests on `/bedrock/model/{id}/invoke` and its streaming sibling with extended thinking on are served through InvokeModel, and `usage.output_tokens_details.thinking_tokens` is reported in unary responses and in the `message_start` and `message_delta` events (#7691)
- **Encrypted Reasoning Retry After a Provider Switch** - The strip-and-retry for replayed reasoning now fires on any 400 that names a reasoning token, instead of matching each provider's wording, so a mid-conversation switch such as bedrock to bedrock_mantle heals instead of returning the 400 to the client. Gemini and Vertex thought signatures carried inside call ids are stripped as well (#7680)
- **OpenRouter Error Messages** - The upstream provider's own error message is lifted out of `error.metadata.raw`, replacing the generic "Provider returned error" (#7680)
- **Truncated Turns on Anthropic and Gemini** - Responses turns cut off by `max_output_tokens` or a refusal now report status `incomplete` with `incomplete_details`, streams end with `response.incomplete`, and the Bedrock, Gemini and Cursor drop-in routes translate that into their own stop reason. A Gemini stream that ends without a finish reason no longer reads as a clean stop (#7677)
- **File Data on OpenAI-Compatible Providers** - `file_data` sent as bare base64 with a `file_type` is folded into a `data:` URL, which OpenAI and Databricks require, instead of being rejected with "Invalid base64 data URL format" (#7682)
- **Gemini Histories Replayed to OpenAI** - A `function_call` input item whose id does not start with `fc` no longer fails OpenAI validation natively or through a fallback. The id is dropped and `call_id` is kept so outputs still pair (#7676)
- **Gemini Thought Signatures on Images and Files** - A thought signature on an inline image or file part stays on that content block and round-trips back to Gemini, instead of being dropped or emitted as a separate reasoning item (#7692)
- **Governance Resets During Startup** - Startup resets and the periodic reset worker now run only after governance state is fully hydrated, and team-owned budgets and rate limits keep the team's calendar alignment after a restart, so calendar-aligned limits are no longer reset on a creation-anchored boundary (#7615, #7637)
- **Model Histogram Unnamed Series** - Rows without a model, such as list_models, file and batch operations, are excluded from the model histogram (#7632)
- **Ungoverned Virtual Key Creation** - A user without an access profile no longer sees locked governance fields when creating a virtual key. The form locks only when a profile actually governs, and a failed policy lookup shows a warning with a retry instead of locking (#7413)

## 🗄️ Database Migrations

- No new database migrations in this release.

## 🐙 Closed GitHub Issues

- [#7538](https://github.com/maximhq/bifrost/issues/7538) - Routing-rule fallbacks are dropped after restart in v2.2.3 (still listed by the API)
- [#7649](https://github.com/maximhq/bifrost/issues/7649) - Bedrock provider drops extended-thinking token count (`output_tokens_details.thinking_tokens`) that AWS returns
