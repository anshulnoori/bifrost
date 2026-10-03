import assert from "node:assert/strict";
import { once } from "node:events";
import { test } from "node:test";
import { createBridge, validate, type Run } from "./bridge.ts";

const token = "synthetic-bridge-token-for-tests-only";
test("account management requires bridge authentication and does not invoke inference", async (t) => {
  let calls = 0;
  const server = createBridge({ token, manage: async () => {
    calls++;
    return Response.json({ state: "disconnected" });
  } }, async () => { throw new Error("not inference"); });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  t.after(() => { server.closeAllConnections(); server.close(); });
  const url = `http://127.0.0.1:${(server.address() as { port: number }).port}/accounts/account-a`;
  assert.equal((await fetch(url)).status, 401);
  assert.equal(calls, 0);
  const response = await fetch(url, { headers: { "x-claude-bridge-token": token } });
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), { state: "disconnected" });
  assert.equal(calls, 1);
});
const request = {
  model: "claude-sonnet-4-6",
  max_tokens: 73,
  system: [{ type: "text", text: "caller instructions", cache_control: { type: "ephemeral" } }],
  messages: [
    { role: "user", content: "first" },
    {
      role: "assistant",
      content: [{ type: "tool_use", id: "t1", name: "lookup", input: { id: 11 } }],
    },
    { role: "user", content: [{ type: "tool_result", tool_use_id: "t1", content: "eleven" }] },
  ],
  tools: [{ name: "lookup", input_schema: { type: "object" } }],
  tool_choice: { type: "any" },
};
async function fixture(t: { after: (fn: () => void) => void }, run: Run, config = {}) {
  const server = createBridge({ token, ...config }, run);
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  t.after(() => {
    server.closeAllConnections();
    server.close();
  });
  const url = `http://127.0.0.1:${(server.address() as { port: number }).port}/v1/messages`;
  return (body: unknown = request, headers: Record<string, string> = {}) =>
    fetch(url, {
      method: "POST",
      headers: { "x-claude-bridge-token": token, "x-claude-bridge-owner": "owner-a", ...headers },
      body: JSON.stringify(body),
    });
}
test("native history, system blocks and caller tools are unchanged", () => {
  assert.equal(validate(request), request);
  for (const invalid of [
    null,
    { ...request, model: "openai/gpt" },
    { ...request, max_tokens: 0 },
    { ...request, messages: [] },
    { ...request, betas: [] },
  ])
    assert.throws(() => validate(invalid));
});
test("admission, exact request forwarding, native errors and independent requests", async (t) => {
  const calls: Parameters<Run>[0][] = [];
  const send = await fixture(t, async (input) => {
    calls.push(input);
    return Response.json(
      { type: "error", error: { type: "rate_limit_error", message: "native limit" } },
      {
        status: 429,
        headers: {
          "retry-after": "37",
          "request-id": "fixture-id",
          "anthropic-ratelimit-tokens-remaining": "11",
          "set-cookie": "do-not-forward",
        },
      },
    );
  });
  assert.equal((await send(request, { "x-claude-bridge-token": "wrong" })).status, 401);
  assert.equal((await send(request, { "x-claude-bridge-owner": "" })).status, 403);
  assert.equal((await send(request, { "x-bifrost-claude-session-id": "old" })).status, 400);
  assert.equal(calls.length, 0);
  const response = await send(request, {
    "anthropic-beta": "caller-beta",
    authorization: "must-not-forward",
  });
  assert.equal(response.status, 429);
  assert.equal(response.headers.get("retry-after"), "37");
  assert.equal(response.headers.get("set-cookie"), null);
  assert.deepEqual(await response.json(), {
    type: "error",
    error: { type: "rate_limit_error", message: "native limit" },
  });
  assert.deepEqual(calls[0].body, request);
  assert.deepEqual(calls[0].headers, { "anthropic-beta": "caller-beta" });
  const next = {
    model: request.model,
    max_tokens: 109,
    messages: [{ role: "user", content: "second only" }],
  };
  await (await send(next)).text();
  assert.deepEqual(calls[1].body, next);
});
test("SSE bytes reach caller before upstream completion", async (t) => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  t.after(release);
  const prefix =
    'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\\\"id\\\":37}"}}\n\n';
  const suffix = 'event: message_stop\ndata: {"type":"message_stop"}\n\n';
  const send = await fixture(
    t,
    async () =>
      new Response(
        new ReadableStream({
          async start(controller) {
            controller.enqueue(new TextEncoder().encode(prefix));
            await gate;
            controller.enqueue(new TextEncoder().encode(suffix));
            controller.close();
          },
        }),
        { headers: { "content-type": "text/event-stream" } },
      ),
  );
  const response = await send({ ...request, stream: true });
  const reader = response.body!.getReader();
  assert.equal(new TextDecoder().decode((await reader.read()).value), prefix);
  release();
  assert.equal(new TextDecoder().decode((await reader.read()).value), suffix);
  assert.equal((await reader.read()).done, true);
});
test("concurrent requests all run at once: no local cap, rejection or queue", async (t) => {
  const burst = 32;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  t.after(release);
  let running = 0;
  let allIn!: () => void;
  const allRunning = new Promise<void>((resolve) => {
    allIn = resolve;
  });
  const send = await fixture(t, async () => {
    // Every request must be inside inference at the same time before any finishes.
    if (++running === burst) allIn();
    await gate;
    return Response.json({ ok: true });
  });
  const responses = Array.from({ length: burst }, () => send());
  await Promise.race([
    allRunning,
    new Promise((_, reject) => setTimeout(() => reject(new Error(`only ${running} of ${burst} ran concurrently`)), 5000)),
  ]);
  release();
  const statuses = await Promise.all(responses.map(async (response) => (await response).status));
  assert.deepEqual(statuses, Array(burst).fill(200));
});
test("each request releases its resources and a later one still runs", async (t) => {
  let calls = 0;
  const send = await fixture(t, async () => {
    calls++;
    return Response.json({ ok: true });
  });
  for (let i = 0; i < 3; i++) assert.equal((await send()).status, 200);
  assert.equal(calls, 3);
});
test("deadline aborts inference and redacts internal failures", async (t) => {
  let aborted = false;
  const send = await fixture(
    t,
    async ({ signal }) => {
      await once(signal, "abort");
      aborted = true;
      throw new Error("private diagnostics");
    },
    { timeoutMs: 25 },
  );
  const response = await send();
  assert.equal(response.status, 504);
  assert.equal(aborted, true);
  assert.doesNotMatch(await response.text(), /private diagnostics/);
});
