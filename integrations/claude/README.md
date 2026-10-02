# Claude

Claude is a subscription-backed native Messages provider. Amp owns context,
tools, and the agent loop. There is no Agent SDK query or CLIProxyAPI service.

## Runtime

The package preserves the original Claude Code executable for login. At build
time, `patch.mjs` replaces only its Bun entrypoint with `entry.js` and the
compiled `bridge.ts`. The replacement calls the binary's existing authenticated
Messages client. It does not start the Claude Code agent loop.

The patch accepts only Claude Code 2.1.287 from the Linux x64 glibc native npm
package 0.3.287, with SHA-256
`3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0`.
Other builds fail closed. Upgrades require inspection of module names, entrypoint
layout, authentication, and synthetic request tests. ARM is not supported yet.

Each request supplies all history, system blocks, tool definitions, and content.
The bridge forwards native Messages fields without adding prompts, tools,
attachments, memory, session history, or compaction. It does not execute tools.
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
used as subscription credentials. The deployment serves one account owner;
virtual keys do not create independent Claude accounts.

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
Run the original binary's `auth login` under that service identity and directory.
The gateway also needs the shared bridge token in its environment file.
Do not configure `ANTHROPIC_API_KEY` for subscription use.

The bridge binds loopback, limits concurrency to four, accepts up to 16 MiB,
and applies a five-minute deadline. It aborts inference on client disconnect.
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
The gateway test is skipped unless a gateway binary/package is supplied.

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
