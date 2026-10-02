import { spawn, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdir, rm } from "node:fs/promises";
import { join } from "node:path";
import type { Manage, Run } from "./bridge.ts";

// Native credential/config caches are process-global. One worker per account is
// deliberate: neither changing an env var nor swapping files isolates accounts.
export function createAccounts(root: string, executable: string) {
  type Worker = { child: ChildProcess; ready: Promise<string>; token: string };
  const workers = new Map<string, Worker>();
  const operations = new Map<string, Promise<unknown>>();
  const accountPattern = /^[a-zA-Z0-9_-]{1,128}$/;
  let closing = false;
  async function stop(worker: Worker) {
    if (worker.child.exitCode !== null || worker.child.signalCode !== null) return;
    await new Promise<void>((resolve) => {
      worker.child.once("exit", () => { clearTimeout(timer); resolve(); });
      const timer = setTimeout(() => worker.child.kill("SIGKILL"), 5000);
      worker.child.kill("SIGTERM");
    });
  }
  async function get(account: string): Promise<Worker> {
    if (!accountPattern.test(account) || closing) throw new Error("invalid account");
    const existing = workers.get(account);
    if (existing) return existing;
    if (workers.size >= 32) throw new Error("Claude account worker limit reached");
    const directory = join(root, "accounts", account);
    // Reserve the slot before yielding; concurrent requests cannot spawn a
    // second worker for the same credential directory.
    const token = randomBytes(32).toString("hex");
    let child!: ChildProcess;
    const worker: Worker = { get child() { return child; }, token, ready: Promise.resolve("") };
    worker.ready = (async () => {
      await mkdir(directory, { recursive: true, mode: 0o700 });
      const env = { ...process.env, HOME: directory, CLAUDE_CONFIG_DIR: directory,
        CLAUDE_BRIDGE_WORKER: "1", CLAUDE_BRIDGE_PORT: "0", CLAUDE_BRIDGE_TOKEN: token };
      // Do not silently adopt a deployment's API key, injected OAuth token,
      // alternate provider, token fd, or native credential-helper configuration.
      for (const name of Object.keys(env))
        if (/^(ANTHROPIC_|CLAUDE_CODE_)/.test(name)) delete (env as Record<string, string | undefined>)[name];
      (env as Record<string, string>).CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1";
      child = spawn(executable, [], { env, cwd: directory, stdio: ["ignore", "pipe", "ignore"] });
      return await new Promise<string>((resolve, reject) => {
        let text = "";
        const timer = setTimeout(() => { child.kill("SIGKILL"); reject(new Error("worker startup timed out")); }, 30000);
        child.once("error", () => { clearTimeout(timer); reject(new Error("worker could not start")); });
        child.once("exit", () => {
          clearTimeout(timer);
          if (workers.get(account) === worker) workers.delete(account);
          reject(new Error("worker exited"));
        });
        child.stdout!.on("data", (chunk) => {
          text += chunk.toString();
          if (text.length > 8192) { child.kill("SIGKILL"); return; }
          if (!text.includes("\n")) return;
          try {
            const port = JSON.parse(text.split("\n")[0]).port;
            if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error();
            clearTimeout(timer);
            child.stdout!.removeAllListeners("data");
            child.stdout!.resume();
            resolve(`http://127.0.0.1:${port}`);
          } catch { child.kill("SIGKILL"); }
        });
      });
    })();
    workers.set(account, worker);
    try { await worker.ready; return worker; }
    catch (error) { if (workers.get(account) === worker) workers.delete(account); throw error; }
  }
  async function request(account: string, path: string, options: RequestInit) {
    await operations.get(account);
    const worker = await get(account);
    const base = await worker.ready;
    return fetch(base + path, { ...options, redirect: "error", headers: {
      ...options.headers, "x-claude-bridge-token": worker.token,
    } });
  }
  const manage: Manage = async (method, path, body) => {
    const match = /^\/accounts\/([a-zA-Z0-9_-]{1,128})(\/start|\/code)?$/.exec(path);
    if (!match) return Response.json({ error: "not found" }, { status: 404 });
    const account = match[1];
    if (method === "DELETE" && !match[2]) {
      const previous = operations.get(account);
      const operation = (async () => {
        await previous;
        const worker = workers.get(account);
        if (worker) { await worker.ready.catch(() => {}); await stop(worker); workers.delete(account); }
        // Kill before deletion: a late token exchange/refresh cannot recreate
        // credentials after disconnect. Requests wait until this completes.
        await rm(join(root, "accounts", account), { recursive: true, force: true });
      })();
      operations.set(account, operation);
      try { await operation; } finally { if (operations.get(account) === operation) operations.delete(account); }
      return Response.json({ state: "disconnected" });
    }
    return request(account, path, { method, signal: AbortSignal.timeout(35000),
      ...(method === "POST" ? { body: JSON.stringify(body), headers: { "content-type": "application/json" } } : {}) });
  };
  const run: Run = async ({ account, body, headers, signal }) => {
    if (!account) throw new Error("account required");
    const status = await request(account, `/accounts/${account}`, { method: "GET", signal });
    if (!status.ok || (await status.json()).state !== "connected")
      return Response.json({ type: "error", error: { type: "authentication_error", message: "Connect this Claude account in the dashboard" } }, { status: 401 });
    return request(account, "/v1/messages", { method: "POST", signal, body: JSON.stringify(body),
      headers: { ...headers, "content-type": "application/json", "x-claude-bridge-owner": "gateway" } });
  };
  return { manage, run, async close() {
    closing = true;
    await Promise.all([...workers.values()].map(async (worker) => { await worker.ready.catch(() => {}); await stop(worker); }));
  } };
}
