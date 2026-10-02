import { createHash, timingSafeEqual } from "node:crypto";
import { once } from "node:events";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";

export type NativeRequest = Record<string, unknown> & {
  model: string;
  max_tokens: number;
  messages: unknown[];
  stream?: boolean;
};
export type Run = (input: {
  body: NativeRequest;
  headers: Record<string, string>;
  signal: AbortSignal;
}) => Promise<Response>;

class RequestError extends Error {
  readonly status: number;
  constructor(message: string, status = 400) {
    super(message);
    this.status = status;
  }
}

export function validate(value: unknown): NativeRequest {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new RequestError("expected a Messages request object");
  const r = value as NativeRequest;
  if (typeof r.model !== "string" || !/^claude-[a-zA-Z0-9.-]+$/.test(r.model))
    throw new RequestError("expected a native Claude model ID");
  if (!Number.isSafeInteger(r.max_tokens) || r.max_tokens < 1)
    throw new RequestError("max_tokens must be a positive integer");
  if (r.stream !== undefined && typeof r.stream !== "boolean")
    throw new RequestError("stream must be boolean");
  if (!Array.isArray(r.messages) || r.messages.length === 0)
    throw new RequestError("messages must be a nonempty array");
  // The upstream validates native content and controls. Do not rewrite schemas,
  // history, thinking blocks, cache controls, or multimodal content here.
  if (Object.hasOwn(r, "betas"))
    throw new RequestError(
      "send beta features in the anthropic-beta header, not an SDK betas field",
    );
  return r;
}

function sendError(res: ServerResponse, status: number, message: string) {
  const types: Record<number, string> = {
    400: "invalid_request_error",
    401: "authentication_error",
    403: "permission_error",
    413: "request_too_large",
    429: "rate_limit_error",
  };
  const payload = { type: "error", error: { type: types[status] ?? "api_error", message } };
  if (res.destroyed || res.writableEnded) return;
  if (res.headersSent) res.end(`event: error\ndata: ${JSON.stringify(payload)}\n\n`);
  else res.writeHead(status, { "content-type": "application/json" }).end(JSON.stringify(payload));
}

export function createBridge(
  config: { token: string; maxConcurrent?: number; timeoutMs?: number },
  run: Run,
) {
  if (config.token.length < 32)
    throw new Error("CLAUDE_BRIDGE_TOKEN must contain at least 32 characters");
  const digest = (s: string) => createHash("sha256").update(s).digest();
  const expected = digest(config.token);
  let active = 0;
  const server = createServer(async (req: IncomingMessage, res: ServerResponse) => {
    const presented = req.headers["x-claude-bridge-token"];
    if (typeof presented !== "string" || !timingSafeEqual(digest(presented), expected)) {
      sendError(res, 401, "bridge authentication required");
      return;
    }
    if (req.method === "GET" && req.url === "/health") {
      res.end("ok");
      return;
    }
    if (req.method !== "POST" || req.url !== "/v1/messages") {
      sendError(res, 404, "not found");
      return;
    }
    if (active >= (config.maxConcurrent ?? 4)) {
      sendError(res, 429, "Claude concurrency limit reached");
      return;
    }
    active++;
    const controller = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
      sendError(res, 504, "Claude request timed out");
      if (!req.complete) req.destroy();
    }, config.timeoutMs ?? 300000);
    const cancel = () => {
      if (!res.writableEnded) controller.abort();
    };
    res.on("close", cancel);
    try {
      const owner = req.headers["x-claude-bridge-owner"];
      if (typeof owner !== "string" || !owner.length || owner.length > 256)
        throw new RequestError("admitted virtual-key owner required", 403);
      if (req.headers["x-bifrost-claude-session-id"] !== undefined)
        throw new RequestError("session continuation is not supported");
      const chunks: Buffer[] = [];
      let size = 0;
      for await (const chunk of req) {
        size += chunk.length;
        if (size > 16 * 1024 * 1024) throw new RequestError("Messages request exceeds 16 MiB", 413);
        chunks.push(chunk);
      }
      let parsed: unknown;
      try {
        parsed = JSON.parse(Buffer.concat(chunks).toString());
      } catch {
        throw new RequestError("invalid JSON body");
      }
      const body = validate(parsed);
      const headers: Record<string, string> = {};
      for (const name of ["anthropic-beta", "anthropic-version"]) {
        const value = req.headers[name];
        if (typeof value === "string") headers[name] = value;
      }
      const upstream = await run({ body, headers, signal: controller.signal });
      if (controller.signal.aborted) return;
      const outgoing: Record<string, string> = {};
      upstream.headers.forEach((value, name) => {
        if (
          ["content-type", "request-id", "retry-after", "anthropic-version"].includes(name) ||
          name.startsWith("anthropic-ratelimit-")
        )
          outgoing[name] = value;
      });
      if (body.stream && upstream.ok) outgoing["cache-control"] = "no-cache";
      res.writeHead(upstream.status, outgoing);
      if (upstream.body)
        for await (const chunk of upstream.body) {
          if (!res.write(chunk)) await once(res, "drain", { signal: controller.signal });
        }
      res.end();
    } catch (error) {
      controller.abort();
      sendError(
        res,
        timedOut ? 504 : error instanceof RequestError ? error.status : 502,
        timedOut
          ? "Claude request timed out"
          : error instanceof RequestError
            ? error.message
            : "Claude inference failed",
      );
    } finally {
      clearTimeout(timer);
      res.off("close", cancel);
      active--;
    }
  });
  return server;
}