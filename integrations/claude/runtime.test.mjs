import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { patchBinary } from "./patch.mjs";

for (const authentication of ["api-key", "oauth-token"]) {
  test(
    `pinned raw binary preserves native requests and stops at tool_use (${authentication})`,
    { timeout: 25000 },
    async (t) => {
      const dir = mkdtempSync(join(tmpdir(), "claude-raw-test-"));
      t.after(() => rmSync(dir, { recursive: true, force: true }));
      const original = new URL(
        "./node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude",
        import.meta.url,
      );
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
      assert.deepEqual(captured[0], first);
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
      assert.deepEqual(captured[1], second);
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