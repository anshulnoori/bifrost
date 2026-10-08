import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { setTimeout as delay } from "node:timers/promises";
import { test } from "node:test";
import { nativeBinary, patchBinary } from "./patch.mjs";

const binary =
  process.env.BIFROST_BINARY ??
  (process.env.BIFROST_PACKAGE && join(process.env.BIFROST_PACKAGE, "bin/bifrost-http"));
test(
  "Bifrost governance -> patched Claude -> native inference and live SSE",
  { skip: !binary, timeout: 60000 },
  async (t) => {
    const dir = mkdtempSync(join(tmpdir(), "claude-gateway-test-"));
    t.after(() => rmSync(dir, { recursive: true, force: true }));
    const captured = [];
    let release;
    const gate = new Promise((resolve) => {
      release = resolve;
    });
    t.after(() => release());
    const upstream = createServer(async (req, res) => {
      assert.equal(req.url, "/v1/messages?beta=true");
      assert.equal(req.headers.authorization, "Bearer synthetic-gateway-oauth-token");
      assert.equal(req.headers["x-api-key"], undefined);
      assert.match(req.headers["anthropic-beta"], /caller-beta/);
      assert.match(req.headers["anthropic-beta"], /oauth/);
      assert.equal(req.headers["anthropic-version"], "2099-01-01");
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const body = JSON.parse(Buffer.concat(chunks));
      captured.push(body);
      if (!body.system?.[0]?.text?.startsWith("x-anthropic-billing-header:")) {
        res.writeHead(429, { "content-type": "application/json" }).end(JSON.stringify({
          type: "error",
          error: { type: "rate_limit_error", message: "missing native billing metadata" },
        }));
        return;
      }
      // Longer than the provider's 1-second default_request_timeout_in_seconds.
      if (body.messages[0].content === "SLOW_GENERATION") await delay(2500);
      const message = {
        id: "msg_native",
        type: "message",
        role: "assistant",
        model: body.model,
        content: [{ type: "tool_use", id: "next", name: "lookup", input: { value: 37 } }],
        stop_reason: "tool_use",
        stop_sequence: null,
        usage: { input_tokens: 19, output_tokens: 7 },
      };
      if (!body.stream) {
        res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify(message));
        return;
      }
      res.writeHead(200, { "content-type": "text/event-stream" });
      for (const event of [
        { type: "message_start", message: { ...message, content: [], stop_reason: null } },
        {
          type: "content_block_start",
          index: 0,
          content_block: { type: "tool_use", id: "next", name: "lookup", input: {} },
        },
        {
          type: "content_block_delta",
          index: 0,
          delta: { type: "input_json_delta", partial_json: '{"value":37}' },
        },
        { type: "content_block_stop", index: 0 },
        {
          type: "message_delta",
          delta: { stop_reason: "tool_use", stop_sequence: null },
          usage: { output_tokens: 7 },
        },
        { type: "message_stop" },
      ]) {
        res.write(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`);
        if (event.type === "content_block_delta") await gate;
      }
      res.end();
    });
    upstream.listen(0, "127.0.0.1");
    await once(upstream, "listening");
    t.after(() => {
      upstream.closeAllConnections();
      upstream.close();
    });
    const token = "synthetic-bridge-token-for-tests-only";
    const patched = join(dir, "claude-raw");
    patchBinary(
      process.env.BIFROST_PACKAGE ? join(process.env.BIFROST_PACKAGE, "bin/claude") : nativeBinary,
      patched,
    );
    const executable = process.env.BIFROST_PACKAGE
      ? join(process.env.BIFROST_PACKAGE, "bin/bifrost-claude")
      : patched;
    const bridge = spawn(executable, [], {
      env: {
        PATH: process.env.PATH,
        HOME: dir,
        CLAUDE_CONFIG_DIR: dir,
        CLAUDE_BRIDGE_TOKEN: token,
        CLAUDE_BRIDGE_PORT: "0",
      CLAUDE_BRIDGE_WORKER: "1",
        CLAUDE_CODE_OAUTH_TOKEN: "synthetic-gateway-oauth-token",
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
        ANTHROPIC_BASE_URL: `http://127.0.0.1:${upstream.address().port}`,
      },
    });
    let diagnostics = "";
    bridge.stderr.on("data", (chunk) => {
      diagnostics += chunk;
    });
    t.after(async () => {
      if (bridge.exitCode === null) {
        bridge.kill("SIGTERM");
        await once(bridge, "exit");
      }
    });
    const lines = createInterface({ input: bridge.stdout });
    const [line] = await Promise.race([
      once(lines, "line"),
      once(bridge, "exit").then(() => {
        throw new Error(diagnostics);
      }),
    ]);
    lines.close();
    const bridgePort = JSON.parse(line).port;
    const listener = net.createServer().listen(0, "127.0.0.1");
    await once(listener, "listening");
    const port = listener.address().port;
    await new Promise((resolve) => listener.close(resolve));
    writeFileSync(
      join(dir, "config.json"),
      JSON.stringify({
        client_config: { enforce_auth_on_inference: true },
        encryption_key: "a".repeat(64),
        config_store: { enabled: true, type: "sqlite", config: { path: join(dir, "config.db") } },
        providers: {
          claude: {
            keys: [{ id: "account-a", name: "Claude fixture", weight: 1, models: ["*"], enabled: true }],
            // The request timeout must not cap Claude; SLOW_GENERATION outlasts it.
            network_config: { base_url: `http://127.0.0.1:${bridgePort}`, max_retries: 0, default_request_timeout_in_seconds: 1 },
          },
        },
        governance: {
          auth_config: {
            is_enabled: true,
            admin_username: "fixture",
            admin_password: "synthetic-local-password",
          },
          virtual_keys: [
            {
              id: "owner-a",
              name: "owner-a",
              value: "sk-bf-fixture-owner",
              is_active: true,
              provider_configs: [
                { provider: "claude", allowed_models: ["claude-sonnet-4-6"], key_ids: ["account-a"], weight: 1 },
              ],
            },
          ],
        },
        plugins: [{ name: "telemetry", enabled: false }],
      }),
    );
    const gateway = spawn(
      binary,
      ["-app-dir", dir, "-host", "127.0.0.1", "-port", String(port), "-log-level", "error"],
      {
        env: { PATH: process.env.PATH, HOME: dir, CLAUDE_BRIDGE_TOKEN: token },
      },
    );
    gateway.stderr.on("data", (chunk) => {
      diagnostics += chunk;
    });
    gateway.stdout.on("data", (chunk) => {
      diagnostics += chunk;
    });
    t.after(async () => {
      if (gateway.exitCode === null) {
        gateway.kill("SIGTERM");
        await once(gateway, "exit");
      }
    });
    const origin = `http://127.0.0.1:${port}`;
    let ready = false;
    for (let i = 0; i < 450; i++) {
      assert.equal(gateway.exitCode, null, diagnostics);
      try {
        if ((await fetch(origin + "/health")).ok) {
          ready = true;
          break;
        }
      } catch {}
      await delay(100);
    }
    assert.ok(ready, diagnostics);
    const management = (path = "/current", options = {}) => fetch(origin + "/api/claude/connections" + path, {
      ...options, headers: { "x-bf-claude-key": "account-a", ...options.headers },
    });
    assert.equal((await management("/current", { headers: { "x-bf-vk": "sk-bf-fixture-owner" } })).status, 401,
      "inference authentication must not manage subscription credentials");
    assert.equal((await management("/usage", { headers: { "x-bf-vk": "sk-bf-fixture-owner" } })).status, 401,
      "inference authentication must not read subscription usage");
    const signedIn = await fetch(origin + "/api/session/login", { method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify({ username: "fixture", password: "synthetic-local-password" }) });
    assert.equal(signedIn.status, 200, await signedIn.clone().text());
    const cookie = signedIn.headers.getSetCookie().map((value) => value.split(";")[0]).join("; ");
    const current = await management("/current", { headers: { cookie } });
    assert.equal(current.status, 200, await current.clone().text());
    assert.equal(current.headers.get("cache-control"), "no-store");
    assert.doesNotMatch(await current.text(), /accessToken|refreshToken|synthetic-gateway-oauth-token/);
    const usage = await management("/usage", { headers: { cookie } });
    const usageText = await usage.text();
    assert.equal(usage.status, 200, usageText);
    assert.equal(usage.headers.get("cache-control"), "no-store");
    // The native reader calls api.anthropic.com only for tokens with the profile
    // scope; this synthetic token lacks it, so no allowance is reported or invented.
    const { checked_at: checkedAt, ...windows } = JSON.parse(usageText);
    assert.deepEqual(windows, {});
    assert.ok(Date.now() - Date.parse(checkedAt) < 60000, "reading time reported");
    assert.equal((await management("", { method: "POST", headers: { cookie, "sec-fetch-site": "cross-site" } })).status, 403);
    assert.equal((await management("/current", { headers: { cookie, "x-bf-claude-key": "unknown-account" } })).status, 404);
    const started = await management("", { method: "POST", headers: { cookie } });
    assert.equal(started.status, 200, await started.clone().text());
    const login = await started.json();
    assert.equal(login.state, "pending");
    assert.equal(new URL(login.authorization_url).origin, "https://claude.com");
    const invalidCode = await management("/code", { method: "POST", headers: { cookie, "content-type": "application/json" },
      body: JSON.stringify({ id: login.id, code: "fake#invalid-state" }) });
    assert.equal(invalidCode.status, 400);
    assert.doesNotMatch(await invalidCode.text(), /fake#|invalid-state|synthetic-gateway-oauth-token/);
    const first = {
      model: "claude/claude-sonnet-4-6",
      max_tokens: 73,
      system: [{ type: "text", text: "ONLY_CALLER_SYSTEM" }],
      messages: [
        { role: "user", content: "FIRST" },
        {
          role: "assistant",
          content: [{ type: "tool_use", id: "old", name: "lookup", input: { value: 11 } }],
        },
        { role: "user", content: [{ type: "tool_result", tool_use_id: "old", content: "ELEVEN" }] },
      ],
      tools: [
        {
          name: "lookup",
          input_schema: { type: "object", properties: { value: { type: "integer" } } },
        },
      ],
      tool_choice: { type: "any" },
    };
    const send = (body, headers = {}, path = "/anthropic/v1/messages") =>
      fetch(origin + path, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "anthropic-beta": "caller-beta",
          "anthropic-version": "2099-01-01",
          "x-bf-vk": "sk-bf-fixture-owner",
          ...headers,
        },
        body: JSON.stringify(body),
      });
    const unauthenticated = await send(first, { "x-bf-vk": "" });
    assert.ok(
      [401, 403].includes(unauthenticated.status),
      `${unauthenticated.status}: ${await unauthenticated.text()}`,
    );
    assert.notEqual((await send({ ...first, model: "claude/claude-opus-4-6" })).status, 200);
    assert.notEqual((await send(first, { "x-bifrost-claude-session-id": "old" })).status, 200);
    assert.notEqual((await send(first, {}, "/v1/chat/completions")).status, 200);
    assert.equal(captured.length, 0, "blocked requests must not reach inference");
    const unary = await send(first);
    assert.equal(unary.status, 200, await unary.clone().text());
    const result = await unary.json();
    assert.equal(result.stop_reason, "tool_use");
    assert.deepEqual(result.content, [
      { type: "tool_use", id: "next", name: "lookup", input: { value: 37 } },
    ]);
    const [billing, ...system] = captured[0].system;
    assert.match(billing.text, /^x-anthropic-billing-header: cc_version=2\.1\.287\.2bf;/);
    assert.deepEqual({ ...captured[0], system }, { ...first, model: "claude-sonnet-4-6" });
    const second = {
      model: first.model,
      max_tokens: 109,
      messages: [{ role: "user", content: "SECOND_ONLY" }],
      stream: true,
    };
    const streamed = await send(second);
    if (streamed.status !== 200) assert.fail(await streamed.text());
    const reader = streamed.body.getReader();
    let output = "";
    while (!output.includes("input_json_delta")) {
      const { value, done } = await reader.read();
      assert.equal(done, false);
      output += new TextDecoder().decode(value);
    }
    assert.doesNotMatch(output, /message_stop/);
    release();
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      output += new TextDecoder().decode(value);
    }
    assert.match(output, /event: message_stop/);
    assert.doesNotMatch(output, /\[DONE\]|response\.output_text/);
    const { system: secondSystem, ...secondBody } = captured[1];
    assert.equal(secondSystem.length, 1, "only billing metadata when caller omits system");
    assert.match(secondSystem[0].text, /^x-anthropic-billing-header: cc_version=2\.1\.287\.642;/);
    assert.deepEqual(secondBody, { ...second, model: "claude-sonnet-4-6" });
    assert.equal(captured.length, 2, "no hidden inference or tool execution");
    const slow = await send({ model: first.model, max_tokens: 31, messages: [{ role: "user", content: "SLOW_GENERATION" }] });
    assert.equal(slow.status, 200, await slow.clone().text());
    assert.equal((await slow.json()).stop_reason, "tool_use");
  },
);
