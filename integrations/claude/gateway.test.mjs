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
import { patchBinary } from "./patch.mjs";

const binary =
  process.env.BIFROST_BINARY ??
  (process.env.BIFROST_PACKAGE && join(process.env.BIFROST_PACKAGE, "bin/bifrost-http"));
test(
  "Bifrost governance -> patched Claude -> native inference and live SSE",
  { skip: !binary, timeout: 30000 },
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
      new URL("./node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude", import.meta.url),
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
            keys: [],
            network_config: { base_url: `http://127.0.0.1:${bridgePort}`, max_retries: 0 },
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
                { provider: "claude", allowed_models: ["claude-sonnet-4-6"], weight: 1 },
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
    for (let i = 0; i < 100; i++) {
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
    assert.deepEqual(captured[0], { ...first, model: "claude-sonnet-4-6" });
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
    assert.deepEqual(captured[1], { ...second, model: "claude-sonnet-4-6" });
    assert.equal(captured.length, 2, "no hidden inference or tool execution");
  },
);