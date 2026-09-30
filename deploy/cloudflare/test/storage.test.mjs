import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { build } from 'esbuild';
import { Miniflare, convertV4MiniflareOptions } from 'miniflare';

test('Durable Object admission survives restarts and serializes concurrent retries', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'bifrost-admission-'));
  let mf;
  const headers = { 'x-deployment-key': 'a'.repeat(64), 'x-deployment-ip': 'b'.repeat(64), 'x-deployment-rpm': '120', 'x-deployment-daily': '1000', 'x-deployment-replay': 'c'.repeat(64) };
  try {
    await build({ entryPoints: ['test/storage-worker.mjs'], bundle: true, format: 'esm', external: ['cloudflare:workers'], outfile: join(dir, 'worker.mjs') });
    const config = { modules: true, modulesRoot: dir, scriptPath: join(dir, 'worker.mjs'), compatibilityDate: '2026-09-23', durableObjects: { LEDGER: { className: 'Ledger', useSQLite: true } }, resourcePersistencePath: join(dir, 'storage') };
    mf = new Miniflare(convertV4MiniflareOptions(config));
    const codes = await Promise.all(Array.from({ length: 32 }, async () => (await mf.dispatchFetch('https://edge.test/', { headers })).status));
    assert.equal(codes.filter(c => c === 200).length, 1);
    assert.equal(codes.filter(c => c === 409).length, 31);
    await mf.dispose();
    mf = new Miniflare(convertV4MiniflareOptions(config));
    assert.equal((await mf.dispatchFetch('https://edge.test/', { headers })).status, 409);
    delete headers['x-deployment-replay'];
    headers['x-deployment-rpm'] = '2';
    assert.equal((await mf.dispatchFetch('https://edge.test/', { headers })).status, 200);
    assert.equal((await mf.dispatchFetch('https://edge.test/', { headers })).status, 429);
  } finally {
    await mf?.dispose();
    await rm(dir, { recursive: true, force: true });
  }
});
