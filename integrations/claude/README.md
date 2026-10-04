# Claude

Claude is a subscription-backed native Messages provider. Amp owns context,
tools, and the agent loop. There is no Agent SDK query or CLIProxyAPI service.

## Runtime

The package preserves the original Claude Code executable. At build
time, `patch.mjs` replaces only its Bun entrypoint with `entry.js` and the
compiled `bridge.ts`. The replacement calls the binary's existing authenticated
Messages client. It does not start the Claude Code agent loop.

The patch accepts only Claude Code 2.1.287 from the Linux glibc native npm
packages 0.3.287. The pinned executable SHA-256 values are:

1. x64: `3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0`.
2. ARM64: `e4daf793d1e74fb0d9874dd09e98690bbfd7be515f78a87fd05b9e2b4bb33b03`.

Other builds fail closed. Upgrades require inspection of module names, entrypoint
layout, authentication, and synthetic request tests. Both architectures use the
same bridge and native interface, with inspected architecture-specific imports.
Optional npm dependencies install the matching host binary. Nix packaging selects
`x86_64-linux` or `aarch64-linux`; it does not rewrite the ELF interpreter.

Each request supplies all history, system blocks, tool definitions, and content.
For OAuth requests, the pinned native helper adds one billing metadata block
before the caller's system blocks. It derives the fingerprint from the caller's
messages and does not add the Claude Code agent prompt.
The bridge preserves caller instructions, history, tools, and attachments.
API-key requests remain unchanged. The bridge does not execute tools or add
memory, session history, or compaction.
Responses and SSE bytes remain native; streaming starts before completion.
The native client adds its transport/authentication headers, including its OAuth
beta. This is not a claim of support for every upstream beta feature.

## Configuration

The public edge exposes `POST /v1/messages`, mapped to Bifrost's internal
`/anthropic/v1/messages`. Use `claude/claude-sonnet-4-6` or another admitted native
model ID. OpenAI endpoints remain for OpenAI/Codex. No cross-protocol conversion
is provided. API-key Anthropic remains a separate existing provider.

```json
{
  "providers": {
    "claude": {
      "keys": [],
      "network_config": {
        "base_url": "http://127.0.0.1:8091",
        "max_retries": 0,
        "default_request_timeout_in_seconds": 300
      }
    }
  }
}
```

Bifrost and the bridge require the same `CLAUDE_BRIDGE_TOKEN`, at least 32
characters. Keep it outside the Nix store. Grant an authenticated virtual key
access to the desired Claude models. Caller authentication headers are never
used as subscription credentials. Create configured Claude accounts in the
dashboard, then grant virtual keys access to those account IDs and models.
Account entries contain routing metadata, not API keys or OAuth credentials.

## Browser sign-in

The Claude Accounts table uses the same layout as Codex/ChatGPT. Add an account
with an optional alias, select Continue, then Connect Claude. Open the Claude
sign-in link and finish authorization in the browser. Copy the complete
`code#state` displayed by Claude into Bifrost and select Complete sign-in.
This uses the pinned binary's native manual browser OAuth flow; the callback
belongs to Claude, not Bifrost. There is no gateway callback redirect to install.

Only authenticated dashboard sessions can manage connections. Inference virtual
keys cannot start login, submit codes, or disconnect accounts. Bifrost validates
the current login attempt and state, and returns display metadata only. Pending
attempts expire after ten minutes. Disconnect removes native credentials; deleting
an account or provider also disconnects its accounts before removing configuration.

Each configured account gets a separate native worker and private directory at
`$CLAUDE_CONFIG_DIR/accounts/<account-ID>`. Process isolation prevents native
credential caches from mixing accounts. Workers do not inherit deployment
Anthropic API keys or Claude OAuth tokens. There is a limit of 32 resident workers
and no idle eviction; restart the bridge to release idle workers.

## Subscription usage

The Claude Accounts table shows the subscription allowance like Codex: a headline
bar (the tighter of the 5-hour session and 7-day windows) and, when expanded, the
5-hour, 7-day, model-specific weekly windows and their reset times. The bridge
reads `GET /api/oauth/usage` through the pinned binary's first-party API client,
with the account's credentials and native refresh. Workers run with
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, which blocks the native allowance
reader. This one request sets `bypassEssentialTrafficOnly`; the worker-wide mode
stays on for every other request.
Anthropic rate-limits this endpoint, so each worker refreshes it at most every
minute, shares one upstream request between concurrent readers, and keeps
serving the last good reading for up to 1 hour. After a 429 the worker waits for
`Retry-After` (5 minutes when absent, at most 1 hour) before asking again; other
failures wait 1 minute. Each reading carries `checked_at`, shown as "Updated …".
The dashboard polls every minute with jitter, and only while the tab is
visible. Codex usage follows the same rules on the gateway, where routing
reserves also read it; a reading older than 1 hour fails closed for routing.
Bifrost returns only window percentages and reset times, never spend, identity,
or token data. Missing or malformed windows are omitted rather than invented, and a
failed read shows "Usage unavailable". This endpoint is undocumented; its shape
was taken from the pinned binary and must be re-inspected on upgrades.

```nix
services.bifrost.claude = {
  enable = true;
  environmentFile = "/run/secrets/bifrost-claude.env";
};
```

The option enables `programs.nix-ld`; it does not rewrite the ELF interpreter.
The monorepo package includes `bin/claude` for original login and
`bin/bifrost-claude` for the patched provider. The service runs as
`bifrost-claude`, with `CLAUDE_CONFIG_DIR=/var/lib/bifrost-claude`.
Use the dashboard flow above; host CLI login is not required. Existing credentials
at the directory root do not automatically connect a configured account.
The gateway also needs the shared bridge token in its environment file.
Do not configure `ANTHROPIC_API_KEY` for subscription use.

The bridge binds loopback, accepts up to 32 MiB (Anthropic's Messages API limit), and applies a five-minute
deadline. It aborts inference on client disconnect. It has no local concurrency
cap or queue: simultaneous requests all run at once. Anthropic enforces each
subscription's real limits, and its errors reach the caller unchanged.
There are no bridge retries or stored conversations. The old
`x-bifrost-claude-session-id` header is rejected. Unknown request fields are
left for upstream validation; the SDK-only `betas` body field is rejected in
favor of the native `anthropic-beta` header.

For development, use Node 24 or newer:

```sh
npm ci
# Set CLAUDE_CONFIG_DIR and CLAUDE_BRIDGE_TOKEN before starting.
npm start
```

## Verification and remaining canaries

```sh
npm test
npm run typecheck
# Include a locally built gateway, or set BIFROST_PACKAGE to its package output.
BIFROST_BINARY=/tmp/bifrost-claude-gateway npm test
```

Tests use local synthetic upstreams and fake tokens, never a live account.
They verify exact history, tool schemas/results, system and image content;
native unary responses; streamed tool-input deltas before completion; no hidden
tool execution; independent requests; admission; deadlines; and build rejection.
They also verify native PKCE login URLs, account isolation, state validation,
dashboard authentication, credential redaction, and cancellation. Browser tests
exercise the account table at narrow and desktop widths and manual-code errors.
The gateway test is skipped unless a gateway binary/package is supplied.
With `BIFROST_PACKAGE` set, native tests patch that package's original `bin/claude`
and run its `bin/bifrost-claude`, rather than a development machine's binary.
The ARM64 native runtime, account isolation/login, unary, SSE, and gateway tests
also pass under QEMU on x64. Emulation does not replace the native NixOS canary.

Saved subscription login, token refresh, real quota behavior, and the NixOS
`nix-ld` service still require a live-account/host canary. Synthetic OAuth proves
the bearer transport path, not acceptance by Anthropic's subscription service.

## Authorization boundary

This implementation deliberately modifies Claude Code. The owner's stated
approval is the basis for proceeding; it is not general authorization for other
deployments. The [Claude Code legal guidance](https://code.claude.com/docs/en/legal-and-compliance)
and [Agent SDK authentication guidance](https://platform.claude.com/docs/en/agent-sdk/overview)
do not by themselves establish permission for a modified subscription gateway.
Do not redistribute the patched binary or offer account access to others unless
the applicable approval covers those actions. Repository tests do not settle
legal authorization or service eligibility.
