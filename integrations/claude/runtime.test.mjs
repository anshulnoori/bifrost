import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { nativeBinary, patchBinary } from "./patch.mjs";

test("native browser login isolates accounts, binds manual state, and cancels without credentials", { timeout: 30000 }, async (t) => {
  const dir = mkdtempSync(join(tmpdir(), "claude-login-test-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const patched = join(dir, "claude-raw");
  patchBinary(process.env.BIFROST_PACKAGE ? join(process.env.BIFROST_PACKAGE, "bin/claude") : nativeBinary, patched);
  const token = "synthetic-account-management-token-only";
  const child = spawn(process.env.BIFROST_PACKAGE ? join(process.env.BIFROST_PACKAGE, "bin/bifrost-claude") : patched, [], {
    env: { PATH: process.env.PATH, HOME: dir, CLAUDE_CONFIG_DIR: dir, CLAUDE_BRIDGE_PORT: "0", CLAUDE_BRIDGE_TOKEN: token,
      // Workers must not adopt these deployment-wide credentials.
      ANTHROPIC_API_KEY: "synthetic-must-not-be-inherited", CLAUDE_CODE_OAUTH_TOKEN: "synthetic-must-not-be-inherited" },
  });
  t.after(async () => { if (child.exitCode === null) { child.kill("SIGTERM"); await once(child, "exit"); } });
  const lines = createInterface({ input: child.stdout });
  const [line] = await once(lines, "line");
  const origin = `http://127.0.0.1:${JSON.parse(line).port}`;
  const action = (account, suffix = "", method = "GET", body) => fetch(`${origin}/accounts/${account}${suffix}`, {
    method, headers: { "x-claude-bridge-token": token }, ...(body ? { body: JSON.stringify(body) } : {}),
  });
  const status = await action("account-a");
  assert.equal(status.status, 200, await status.clone().text());
  assert.deepEqual(await status.json(), { state: "disconnected" });
  // The native usage reader runs only for a connected subscription account.
  const usage = await action("account-a", "/usage");
  assert.equal(usage.status, 409, await usage.clone().text());
  assert.doesNotMatch(await usage.text(), /accessToken|refreshToken|synthetic-must-not/);
  const start = await action("account-a", "/start", "POST");
  assert.equal(start.status, 200, await start.clone().text());
  const login = await start.json();
  assert.equal(login.state, "pending");
  const url = new URL(login.authorization_url);
  assert.equal(url.origin + url.pathname, "https://claude.com/cai/oauth/authorize");
  assert.equal(url.searchParams.get("code_challenge_method"), "S256");
  assert.equal(url.searchParams.get("redirect_uri"), "https://platform.claude.com/oauth/code/callback");
  assert.ok(url.searchParams.get("state"));
  assert.ok(url.searchParams.get("code_challenge"));
  assert.doesNotMatch(JSON.stringify(login), /accessToken|refreshToken|codeVerifier|synthetic-must-not/);
  const repeat = await (await action("account-a", "/start", "POST")).json();
  assert.equal(repeat.id, login.id, "repeated start must keep the active attempt");
  const other = await (await action("account-b", "/start", "POST")).json();
  assert.notEqual(other.id, login.id);
  assert.notEqual(new URL(other.authorization_url).searchParams.get("state"), url.searchParams.get("state"));
  assert.equal((await action("account-a", "/code", "POST", { id: login.id, code: "fake#wrong-state" })).status, 400);
  assert.equal((await action("account-b", "/code", "POST", { id: login.id, code: `fake#${url.searchParams.get("state")}` })).status, 409);
  const disconnected = await action("account-a", "", "DELETE");
  assert.equal(disconnected.status, 200);
  assert.deepEqual(await disconnected.json(), { state: "disconnected" });
  assert.deepEqual(await (await action("account-a")).json(), { state: "disconnected" });
  assert.equal((await (await action("account-b")).json()).id, other.id, "cancel must not affect the other account");
});

test("usage read bypasses essential-traffic mode for that request only", { timeout: 30000 }, async (t) => {
  const dir = mkdtempSync(join(tmpdir(), "claude-usage-test-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const patched = join(dir, "claude-raw");
  patchBinary(process.env.BIFROST_PACKAGE ? join(process.env.BIFROST_PACKAGE, "bin/claude") : nativeBinary, patched);
  writeFileSync(join(dir, ".credentials.json"), JSON.stringify({ claudeAiOauth: {
    accessToken: "synthetic-usage-access-token", refreshToken: "synthetic-usage-refresh-token",
    expiresAt: Date.now() + 3600000, scopes: ["user:inference", "user:profile"], subscriptionType: "pro",
  } }), { mode: 0o600 });
  const requests = [];
  // Plain-HTTP proxy: the native client sends the absolute HTTPS URL, so no TLS is needed.
  const proxy = createServer((req, res) => {
    requests.push({ url: req.url, authorization: req.headers.authorization, beta: req.headers["anthropic-beta"] });
    res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({
      five_hour: { utilization: 71, resets_at: "2026-10-03T01:30:00.226479+00:00" },
      seven_day: { utilization: 12, resets_at: "2026-10-03T16:00:00.2265+00:00" },
    }));
  });
  proxy.listen(0, "127.0.0.1");
  await once(proxy, "listening");
  t.after(() => proxy.close());
  const token = "synthetic-usage-bridge-token-for-tests-only";
  const child = spawn(process.env.BIFROST_PACKAGE ? join(process.env.BIFROST_PACKAGE, "bin/bifrost-claude") : patched, [], {
    env: { PATH: process.env.PATH, HOME: dir, CLAUDE_CONFIG_DIR: dir, CLAUDE_BRIDGE_WORKER: "1", CLAUDE_BRIDGE_PORT: "0",
      CLAUDE_BRIDGE_TOKEN: token, CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
      HTTPS_PROXY: `http://127.0.0.1:${proxy.address().port}`, NO_PROXY: "127.0.0.1,localhost" },
  });
  t.after(async () => { if (child.exitCode === null) { child.kill("SIGTERM"); await once(child, "exit"); } });
  const [line] = await once(createInterface({ input: child.stdout }), "line");
  const usage = await fetch(`http://127.0.0.1:${JSON.parse(line).port}/accounts/account-a/usage`,
    { headers: { "x-claude-bridge-token": token }, signal: AbortSignal.timeout(15000) });
  assert.equal(usage.status, 200, await usage.clone().text());
  assert.deepEqual((await usage.json()).five_hour.utilization, 71);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, "https://api.anthropic.com/api/oauth/usage");
  assert.equal(requests[0].authorization, "Bearer synthetic-usage-access-token");
  assert.match(requests[0].beta, /oauth-2025-04-20/);
});

for (const authentication of ["api-key", "oauth-token"]) {
  test(
    `pinned raw binary preserves native requests and stops at tool_use (${authentication})`,
    { timeout: 25000 },
    async (t) => {
      const dir = mkdtempSync(join(tmpdir(), "claude-raw-test-"));
      t.after(() => rmSync(dir, { recursive: true, force: true }));
      const original = process.env.BIFROST_PACKAGE ? join(process.env.BIFROST_PACKAGE, "bin/claude") : nativeBinary;
      const patched = join(dir, "claude-raw");
      const wrong = join(dir, "wrong-build");
      writeFileSync(wrong, "not the approved pinned executable");
      assert.throws(() => patchBinary(wrong, patched), /unsupported Claude executable/);
      patchBinary(original, patched);
      assert.throws(() => patchBinary(original, patched), { code: "EEXIST" });

      const captured = [];
      let cancelled;
      const cancellation = new Promise((resolve) => {
        cancelled = resolve;
      });
      let release;
      const gate = new Promise((resolve) => {
        release = resolve;
      });
      const assertForwarded = (actual, expected, fingerprint) => {
        if (authentication === "api-key") return assert.deepEqual(actual, expected);
        const [billing, ...system] = actual.system;
        assert.equal(billing.type, "text");
        assert.match(billing.text, new RegExp(`^x-anthropic-billing-header: cc_version=2\\.1\\.287\\.${fingerprint};`));
        assert.deepEqual(system, Array.isArray(expected.system)
          ? expected.system : [{ type: "text", text: expected.system }]);
        const { system: actualSystem, ...actualBody } = actual;
        const { system: expectedSystem, ...expectedBody } = expected;
        assert.deepEqual(actualBody, expectedBody);
      };
      const upstream = createServer(async (req, res) => {
        assert.equal(req.url, "/v1/messages?beta=true");
        if (authentication === "oauth-token") {
          assert.equal(req.headers.authorization, "Bearer sk-ant-oat01-synthetic-raw-fixture-only");
          assert.equal(req.headers["x-api-key"], undefined);
          assert.match(req.headers["anthropic-beta"], /oauth/);
        } else assert.equal(req.headers["x-api-key"], "synthetic-raw-fixture-only");
        let text = "";
        for await (const chunk of req) text += chunk;
        const body = JSON.parse(text);
        captured.push(body);
        if (authentication === "oauth-token" && !body.system?.[0]?.text?.startsWith("x-anthropic-billing-header:")) {
          res.writeHead(429, { "content-type": "application/json" }).end(JSON.stringify({
            type: "error",
            error: { type: "rate_limit_error", message: "missing native billing metadata" },
          }));
          return;
        }
        if (body.messages[0].content === "NATIVE_ERROR") {
          res
            .writeHead(429, {
              "content-type": "application/json",
              "retry-after": "37",
              "request-id": "native-error-id",
            })
            .end(
              JSON.stringify({
                type: "error",
                error: { type: "rate_limit_error", message: "fixture quota" },
              }),
            );
          return;
        }
        if (body.messages[0].content === "CANCEL_INFERENCE") {
          res.on("close", cancelled);
          res.writeHead(200, { "content-type": "text/event-stream" });
          res.write('event: ping\ndata: {"type":"ping"}\n\n');
          return;
        }
        const message = {
          id: `msg_${captured.length}`,
          type: "message",
          role: "assistant",
          model: body.model,
          content: [
            { type: "tool_use", id: "tool_next", name: "caller_tool", input: { value: 37 } },
          ],
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
            content_block: { type: "tool_use", id: "tool_next", name: "caller_tool", input: {} },
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
      t.after(() => {
        release();
        upstream.closeAllConnections();
        upstream.close();
      });
      upstream.listen(0, "127.0.0.1");
      await once(upstream, "listening");
      const executable = process.env.BIFROST_PACKAGE
        ? join(process.env.BIFROST_PACKAGE, "bin/bifrost-claude")
        : patched;
      const child = spawn(executable, [], {
        cwd: dir,
        env: {
          PATH: process.env.PATH,
          HOME: dir,
          CLAUDE_CONFIG_DIR: dir,
          CLAUDE_BRIDGE_MODULE: new URL("./bridge.ts", import.meta.url).pathname,
          CLAUDE_BRIDGE_TOKEN: "synthetic-bridge-token-for-tests-only",
          CLAUDE_BRIDGE_PORT: "0",
        CLAUDE_BRIDGE_WORKER: "1",
          ANTHROPIC_API_KEY: "synthetic-raw-fixture-only",
          ANTHROPIC_BASE_URL: `http://127.0.0.1:${upstream.address().port}`,
          CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
          ...(authentication === "oauth-token"
            ? {
                ANTHROPIC_API_KEY: undefined,
                CLAUDE_CODE_OAUTH_TOKEN: "sk-ant-oat01-synthetic-raw-fixture-only",
              }
            : {}),
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      let stderr = "";
      child.stderr.on("data", (chunk) => {
        stderr += chunk;
      });
      const lines = createInterface({ input: child.stdout });
      t.after(async () => {
        lines.close();
        const exited = child.exitCode !== null ? Promise.resolve() : once(child, "exit");
        child.kill("SIGTERM");
        await exited;
      });
      const [line] = await Promise.race([
        once(lines, "line"),
        once(child, "exit").then(([code]) => {
          throw new Error(`binary exit ${code}: ${stderr}`);
        }),
      ]);
      const url = `http://127.0.0.1:${JSON.parse(line).port}/v1/messages`;
      const tools = [
        {
          name: "caller_tool",
          description: "Caller executes this, not Claude.",
          input_schema: {
            type: "object",
            properties: { value: { type: "integer" } },
            required: ["value"],
          },
        },
      ];
      const first = {
        model: "claude-sonnet-4-6",
        max_tokens: 73,
        system: [{ type: "text", text: "EXACT_CALLER_SYSTEM" }],
        messages: [
          { role: "user", content: "FIRST_CONTEXT_ONLY" },
          {
            role: "assistant",
            content: [
              { type: "tool_use", id: "tool_previous", name: "caller_tool", input: { value: 11 } },
            ],
          },
          {
            role: "user",
            content: [
              { type: "tool_result", tool_use_id: "tool_previous", content: "ANSWER_ELEVEN" },
            ],
          },
        ],
        tools,
        tool_choice: { type: "any" },
        stream: false,
      };
      const headers = {
        "x-claude-bridge-token": "synthetic-bridge-token-for-tests-only",
        "x-claude-bridge-owner": "owner-a",
      };
      const response = await fetch(url, { method: "POST", headers, body: JSON.stringify(first) });
      assert.equal(response.status, 200, await response.clone().text());
      assert.deepEqual((await response.json()).content, [
        { type: "tool_use", id: "tool_next", name: "caller_tool", input: { value: 37 } },
      ]);
      // Independently derived from the pinned fingerprint salt, positions 4/7/20,
      // and version: SHA256("59cf53e54c78TO02.1.287").slice(0, 3).
      assertForwarded(captured[0], first, "bf4");
      const second = {
        model: "claude-sonnet-4-6",
        max_tokens: 109,
        system: "SECOND_SYSTEM_ONLY",
        messages: [
          {
            role: "user",
            content: [
              { type: "text", text: "SECOND_CONTEXT_ONLY" },
              {
                type: "image",
                source: {
                  type: "base64",
                  media_type: "image/png",
                  data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==",
                },
              },
            ],
          },
        ],
        tools,
        stream: true,
      };
      const streamed = await fetch(url, { method: "POST", headers, body: JSON.stringify(second) });
      if (streamed.status !== 200) assert.fail(await streamed.text());
      const reader = streamed.body.getReader();
      let output = "";
      while (!output.includes("input_json_delta")) {
        const { value, done } = await reader.read();
        assert.equal(done, false);
        output += new TextDecoder().decode(value);
      }
      assert.doesNotMatch(output, /event: message_stop/);
      release();
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        output += new TextDecoder().decode(value);
      }
      assert.match(output, /event: message_stop/);
      assert.match(output, /"stop_reason":"tool_use"/);
      // The first user text is inside multimodal content, not a string body.
      assertForwarded(captured[1], second, "764");
      assert.equal(captured.length, 2, "no tool execution or follow-up inference");
      const failed = await fetch(url, {
        method: "POST",
        headers,
        body: JSON.stringify({
          model: first.model,
          max_tokens: 17,
          messages: [{ role: "user", content: "NATIVE_ERROR" }],
        }),
      });
      assert.equal(failed.status, 429);
      assert.equal(failed.headers.get("retry-after"), "37");
      assert.equal(failed.headers.get("request-id"), "native-error-id");
      assert.deepEqual(await failed.json(), {
        type: "error",
        error: { type: "rate_limit_error", message: "fixture quota" },
      });
      const abort = new AbortController();
      const disconnect = await fetch(url, {
        method: "POST",
        headers,
        signal: abort.signal,
        body: JSON.stringify({
          model: first.model,
          max_tokens: 17,
          messages: [{ role: "user", content: "CANCEL_INFERENCE" }],
          stream: true,
        }),
      });
      const disconnectReader = disconnect.body.getReader();
      assert.match(new TextDecoder().decode((await disconnectReader.read()).value), /event: ping/);
      abort.abort();
      await cancellation;
      assert.equal(captured.length, 4, "native error is not retried");
      console.log(
        "Verified exact history, tools, image, system, native streaming, and isolated requests in one process.",
      );
    },
  );
}
