import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHmac, randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { authenticate, consume, signature } from '../src/auth.mjs';
import { handle } from '../src/handler.mjs';

const secret = 'a3'.repeat(32);
const now = 1790136000000;
function signed({ path = '/wake', origin = 'https://wake.example', time = now / 1000,
                  nonce = 'cd'.repeat(16), method = 'POST', body, headers = {} } = {}) {
  // Independent Node crypto/canonical construction, not the production signer.
  const sig = createHmac('sha256', secret)
    .update(['tailnet-lifecycle-v1', origin, 'POST', path, time, nonce].join('\n')).digest('hex');
  return new Request(origin + path, { method, body, headers: {
    'x-wake-time': String(time), 'x-wake-nonce': nonce, 'x-wake-signature': sig, ...headers,
  } });
}
class Storage {
  data;
  constructor(data = new Map()) { this.data = data; }
  async list({ prefix }) { return new Map([...this.data].filter(([k]) => k.startsWith(prefix))); }
  async get(k) { return structuredClone(this.data.get(k)); }
  async put(k, v) { this.data.set(k, structuredClone(v)); }
  async delete(k) { this.data.delete(k); }
}

test('valid independent signature and exact timestamp boundaries', async () => {
  for (const delta of [-30, 0, 30]) {
    assert.equal((await authenticate(signed({ time: now / 1000 + delta }), secret, now)).action, 'wake');
  }
  for (const delta of [-31, 31]) assert.equal(await authenticate(signed({ time: now / 1000 + delta }), secret, now), null);
});
test('signer agrees with independent HMAC implementation', async () => {
  assert.equal(await signature(secret, 'asymmetric message'),
    createHmac('sha256', secret).update('asymmetric message').digest('hex'));
});
test('key, host and action are cryptographically bound', async () => {
  const r = signed();
  for (const url of ['https://other.example/wake', 'https://wake.example/status']) {
    assert.equal(await authenticate(new Request(url, { method: 'POST', headers: r.headers }), secret, now), null);
  }
  assert.equal(await authenticate(r, 'b4'.repeat(32), now), null);
  assert.equal(await authenticate(r, undefined, now), null);
});
test('bodies, methods, query parameters, forwarded ports and arbitrary paths cannot proxy', async () => {
  for (const opts of [
    { body: 'prompt' }, { body: '' }, { method: 'GET' }, { path: '/wake?url=http://localhost:8080' },
    { path: '/v1/chat/completions' }, { path: '/admin' }, { path: '/events' },
    { path: '/metrics' }, { origin: 'http://wake.example' },
    { body: 'unexpected', headers: { 'content-length': '0' } },
    { headers: { 'content-length': '9' } }, { headers: { 'transfer-encoding': 'chunked' } },
    { headers: { 'x-wake-signature': '00'.repeat(32) } },
  ]) assert.equal(await authenticate(signed(opts), secret, now), null, JSON.stringify(opts));
});
test('empty workerd-style POST stream is accepted without allowing body bytes', async () => {
  assert.equal((await authenticate(signed({ body: '', headers: { 'content-length': '0' } }), secret, now)).action, 'wake');
});
test('replay survives storage recreation; timestamp expiry cannot become valid after GC', async () => {
  const s = new Storage();
  const t = await authenticate(signed({ time: now / 1000 + 30 }), secret, now);
  assert.equal(await consume(s, t, now), 200);
  assert.equal(await consume(new Storage(s.data), t, now + 60_000), 409);
  assert.equal(await consume(new Storage(s.data), t, now + 61_000), 401);
});
test('six wakes and sixty total controls per minute; status cannot exhaust wake allowance alone', async () => {
  const s = new Storage();
  const t = { time: now / 1000, nonce: '', action: 'status' };
  for (let i = 0; i < 54; i++) assert.equal(await consume(s, { ...t, nonce: String(i) }, now), 200);
  for (let i = 54; i < 60; i++) assert.equal(await consume(s, { ...t, nonce: String(i), action: 'wake' }, now), 200);
  assert.equal(await consume(s, { ...t, nonce: 'next' }, now), 429);
  assert.equal(await consume(s, { ...t, nonce: 'later', time: (now + 60_000) / 1000 }, now + 60_000), 200);
  assert.ok(s.data.size <= 2); // stale nonces removed
  const wakes = new Storage();
  for (let i = 0; i < 6; i++) assert.equal(await consume(wakes, { ...t, action: 'wake', nonce: String(i) }, now), 200);
  assert.equal(await consume(wakes, { ...t, action: 'wake', nonce: 'seventh' }, now), 429);
});
test('unauthenticated ingress never touches a Durable Object', async () => {
  const env = { WAKE_HMAC_KEY: secret, PROBE: { getByName() { assert.fail('must not reach DO'); } } };
  for (const path of ['/', '/wake', '/status', '/v1/responses', '/admin', '/metrics', '/healthz']) {
    assert.equal((await handle(new Request('https://wake.example' + path), env)).status, 404);
  }
});
test('authenticated handler passes only a lifecycle ticket; strips all container result data', async () => {
  const env = { WAKE_HMAC_KEY: secret, PROBE: { getByName(name) {
    assert.equal(name, 'private-proof');
    return { control(ticket) {
      assert.deepEqual(Object.keys(ticket).sort(), ['action', 'nonce', 'time']);
      return { code: 200, status: 'running', prompt: 'must not escape' };
    } };
  } } };
  const result = await handle(signed({ time: Math.floor(Date.now() / 1000), nonce: randomBytes(16).toString('hex'),
    headers: { 'x-container-port': '8080', authorization: 'do-not-forward' } }), env);
  assert.deepEqual(await result.json(), { status: 'running' });
  assert.equal(result.headers.get('cache-control'), 'no-store');
});
test('shipping config has no routes, public previews, cron wakeups or inherited fetch proxy', () => {
  const config = JSON.parse(readFileSync(new URL('../wrangler.jsonc', import.meta.url)));
  assert.equal(config.workers_dev, false);
  assert.equal(config.preview_urls, false);
  assert.deepEqual(config.routes, []);
  assert.equal(config.triggers, undefined);
  assert.equal(config.containers[0].max_instances, 1);
  const source = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');
  assert.match(source, /override async fetch\(\).*\{\s*return reply\(404\)/);
  assert.doesNotMatch(source, /containerFetch|getTcpPort|super\.fetch/);
  const serve = JSON.parse(readFileSync(new URL('../container/serve.json', import.meta.url)));
  assert.deepEqual(serve.AllowFunnel, {});
  assert.deepEqual(Object.keys(serve.TCP), ['443']);
  assert.equal(serve.Web['${TS_CERT_DOMAIN}:443'].Handlers['/'].Proxy, 'http://127.0.0.1:8080');
});
