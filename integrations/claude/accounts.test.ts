import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { createAccounts } from "./accounts.ts";

// A stand-in worker: reports its port, then answers every request with its account directory.
function fakeWorker(dir: string) {
  const path = join(dir, "worker.mjs");
  writeFileSync(path, `#!/usr/bin/env node
import { createServer } from "node:http";
const server = createServer((req, res) => {
  res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({ state: "connected", home: process.env.CLAUDE_CONFIG_DIR }));
});
server.listen(0, "127.0.0.1", () => console.log(JSON.stringify({ port: server.address().port })));
process.on("SIGTERM", () => server.close(() => process.exit(0)));
`);
  chmodSync(path, 0o755);
  return path;
}

test("every connected account gets its own worker, with no local account cap", { timeout: 60000 }, async (t) => {
  const dir = mkdtempSync(join(tmpdir(), "claude-accounts-test-"));
  const accounts = createAccounts(dir, fakeWorker(dir));
  t.after(async () => {
    await accounts.close();
    rmSync(dir, { recursive: true, force: true });
  });
  // More accounts than the old hardcoded limit of 32.
  const ids = Array.from({ length: 40 }, (_, i) => `account-${i}`);
  const homes = await Promise.all(ids.map(async (id) => {
    const response = await accounts.manage("GET", `/accounts/${id}`, {});
    assert.equal(response.status, 200, await response.clone().text());
    return (await response.json()).home;
  }));
  // Each account runs in its own isolated credential directory.
  assert.deepEqual(homes, ids.map((id) => join(dir, "accounts", id)));
});
