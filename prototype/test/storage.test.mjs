import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHmac } from 'node:crypto';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { build } from 'esbuild';
import { Miniflare, convertV4MiniflareOptions } from 'miniflare';

test('real SQLite transactions reject concurrent replay and replay after runtime restart', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'wake-storage-'));
  const secret = 'e7'.repeat(32);
  const time = String(Math.floor(Date.now() / 1000));
  const nonce = 'a9'.repeat(16);
  const sig = createHmac('sha256', secret).update(
    ['tailnet-lifecycle-v1', 'https://wake.example', 'POST', '/status', time, nonce].join('\n'),
  ).digest('hex');
  const opts = { method: 'POST', headers: {
    'content-length': '0',
    'x-wake-time': time, 'x-wake-nonce': nonce, 'x-wake-signature': sig,
  } };
  let mf;
  try {
    await build({ entryPoints: ['test/ledger-worker.mjs'], bundle: true, format: 'esm',
      external: ['cloudflare:workers'], outfile: join(dir, 'worker.mjs') });
    const config = { modules: true, modulesRoot: dir, scriptPath: join(dir, 'worker.mjs'),
      compatibilityDate: '2026-09-23', bindings: { WAKE_HMAC_KEY: secret },
      durableObjects: { PROBE: { className: 'Ledger', useSQLite: true } },
      resourcePersistencePath: join(dir, 'storage') };
    mf = new Miniflare(convertV4MiniflareOptions(config));
    const responses = await Promise.all(Array.from({ length: 20 }, () => mf.dispatchFetch('https://wake.example/status', opts)));
    assert.equal(responses.filter(r => r.status === 200).length, 1, JSON.stringify(responses.map(r => r.status)));
    assert.equal(responses.filter(r => r.status === 409).length, 19);
    await mf.dispose();
    mf = new Miniflare(convertV4MiniflareOptions(config));
    assert.equal((await mf.dispatchFetch('https://wake.example/status', opts)).status, 409);
  } finally {
    await mf?.dispose();
    await rm(dir, { recursive: true, force: true });
  }
});
