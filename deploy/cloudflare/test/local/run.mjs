import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { importPKCS8, SignJWT } from 'jose';
import { randomUUID } from 'node:crypto';

const infer = 'http://127.0.0.1:8791', admin = 'http://127.0.0.1:8792', control = 'http://127.0.0.1:8793';
const privateKey = await importPKCS8(await readFile(new URL('../../.wrangler/local-e2e/signing-key.pem', import.meta.url), 'utf8'), 'RS256');
const jwt = async (claims = {}, audience = 'fixture-audience', expires = '5m') => new SignJWT({ email: 'owner@example.test', type: 'app', ...claims })
  .setProtectedHeader({ alg: 'RS256', kid: 'local-fixture' }).setIssuer('https://fixture.cloudflareaccess.com')
  .setAudience(audience).setSubject('fixture-owner').setIssuedAt().setExpirationTime(expires).sign(privateKey);
const key = name => `sk-bf-fixture-${name}-` + 'x'.repeat(32);
const post = (body = { model: 'fixture/model', input: 'synthetic' }, extra = {}, name = 'normal', signal) => fetch(infer + '/v1/responses', {
  method: 'POST', headers: { authorization: `Bearer ${key(name)}`, 'content-type': 'application/json', ...extra }, body: JSON.stringify(body), signal,
});
const state = async () => {
  const response = await fetch(control + '/status');
  assert.equal(response.status, 200, 'local lifecycle control binding');
  return response.json();
};
const action = async name => assert.equal((await fetch(control + '/' + name, { method: 'POST' })).status, 200);
async function until(predicate, label, ms = 15000) {
  const start = Date.now();
  while (Date.now() - start < ms) { if (await predicate()) return; await new Promise(r => setTimeout(r, 100)); }
  assert.fail(label);
}
let checks = 0;
const pass = label => { checks++; console.log(`PASS ${label}`); };

try {
  await action('stop');
  await until(async () => (await state()).state.status === 'stopped', 'initial stop');
  await action('reset');
  const forbidden = ['/health', '/metrics', '/api/providers', '/api/plugins', '/api/headroom/events', '/v1/compress', '/debug/pprof', '/wake', '/status', '/unknown', '/v1/models', '/v1/responses/x', '/v1/%72esponses'];
  for (const path of forbidden) {
    assert.equal((await fetch(infer + path, { method: 'POST', headers: { authorization: `Bearer ${key('normal')}` }, body: '{}' })).status, 404, path);
  }
  assert.equal((await fetch(infer + '/v1/responses', { method: 'POST', body: '{}' })).status, 401);
  assert.equal((await post({ model: 'forbidden' })).status, 403);
  assert.equal((await post(undefined, { authorization: 'Bearer invalid' })).status, 401);
  assert.equal((await fetch(infer + '/v1/responses', { method: 'POST', headers: { authorization: `Bearer ${key('normal')}`, 'content-type': 'application/json' }, body: '{broken' })).status, 400);
  assert.equal((await fetch(infer + '/v1/responses', { method: 'POST', headers: { authorization: `Bearer ${key('normal')}`, 'content-type': 'application/json' },
    body: new ReadableStream({ start(c) { c.enqueue(new Uint8Array(4 * 1024 * 1024 + 1)); c.close(); } }), duplex: 'half' })).status, 413);
  assert.equal((await fetch(admin + '/api/providers', { headers: { 'cf-access-jwt-assertion': 'spoof', 'cf-access-authenticated-user-email': 'owner@example.test' } })).status, 403);
  assert.equal((await state()).state.status, 'stopped');
  pass('allowlists, malformed input, inference auth and spoofed Access headers do not wake Docker');

  const started = Date.now();
  const cold = await post(undefined, { 'x-deployment-plane': 'admin', 'x-bf-api-key': 'synthetic', cookie: 'token=fixture-session' });
  assert.equal(cold.status, 200, 'authenticated cold wake');
  const first = await cold.json();
  assert.equal(first.path, '/v1/responses');
  assert.equal(cold.headers.get('set-cookie'), null);
  assert.ok(cold.headers.get('x-request-id'));
  for (const name of ['x-deployment-plane', 'x-bf-api-key', 'cookie', 'authorization']) assert.ok(!first.headerNames.includes(name), name);
  assert.equal((await state()).state.status, 'healthy');
  pass(`authenticated readiness/wake and header stripping (${Date.now() - started}ms local Docker, not cloud cold-start latency)`);

  for (const [path, upstream] of [['/v1/chat/completions', '/v1/chat/completions'], ['/v1/messages', '/anthropic/v1/messages']]) {
    const response = await fetch(infer + path, { method: 'POST', headers: { 'x-api-key': key('normal'), 'content-type': 'application/json' },
      body: '{"model":"fixture/model","messages":[{"role":"user","content":"synthetic"}]}' });
    assert.equal(response.status, 200);
    assert.equal((await response.json()).path, upstream);
  }

  const replay = randomUUID();
  const results = await Promise.all(Array.from({ length: 8 }, () => post(undefined, { 'idempotency-key': replay }).then(r => r.status)));
  assert.equal(results.filter(x => x === 200).length, 1);
  assert.equal(results.filter(x => x === 409).length, 7);
  for (const expected of [200, 200, 429]) assert.equal((await post(undefined, {}, 'limited')).status, expected);
  pass('concurrent replay serialization and per-key rate limits over HTTP');

  for (const token of [await jwt({ email: 'intruder@example.test' }), await jwt({}, 'wrong'), await jwt({}, undefined, '-2m')]) {
    assert.equal((await fetch(admin + '/api/providers', { headers: { 'cf-access-jwt-assertion': token } })).status, 403);
  }
  const token = await jwt();
  const access = { 'cf-access-jwt-assertion': token };
  assert.equal((await fetch(admin + '/api/providers', { headers: access })).status, 401, 'Bifrost session mock remains required');
  const session = { ...access, cookie: 'CF_Authorization=synthetic; token=fixture-session', 'cf-access-authenticated-user-email': 'spoof' };
  const accepted = await fetch(admin + '/api/providers', { headers: session });
  assert.equal(accepted.status, 200, 'private AdminOrigin service binding');
  const seen = await accepted.json();
  assert.equal(seen.session, true);
  for (const name of ['cf-access-jwt-assertion', 'cf-access-authenticated-user-email']) assert.ok(!seen.headerNames.includes(name), name);
  assert.equal((await fetch(admin + '/api/providers', { headers: { ...session, authorization: `Bearer ${key('normal')}` } })).status, 403);
  assert.equal((await fetch(admin + '/api/providers', { method: 'POST', headers: { ...session, origin: 'https://evil.example' }, body: '{}' })).status, 403);
  for (const path of ['/metrics', '/health', '/api/plugins', '/v1/responses', '/api/headroom/events']) assert.equal((await fetch(admin + path, { headers: session })).status, 404);
  pass('cryptographic Access claims, private admin binding, session defense, CSRF and header stripping');

  const complete = await post({ model: 'fixture/model', input: 'synthetic', stream: true });
  assert.equal(complete.headers.get('content-type'), 'text/event-stream');
  assert.match(await complete.text(), /data: \[DONE\]\n\n$/);
  await until(async () => (await state()).leases === 0, 'SSE EOF lease release');
  const abort = new AbortController();
  const stream = await post({ model: 'fixture/model', input: 'hold', stream: true }, {}, 'normal', abort.signal);
  const reader = stream.body.getReader();
  let event = '';
  const decoder = new TextDecoder();
  while (!event.includes('\n\n')) {
    const chunk = await reader.read();
    assert.equal(chunk.done, false, 'stream must deliver its first event');
    event += decoder.decode(chunk.value, { stream: true });
  }
  assert.match(event, /"sequence":1/);
  assert.equal((await state()).leases, 1);
  await action('idle');
  assert.equal((await state()).state.status, 'healthy', 'idle check cannot stop active stream');
  abort.abort();
  await reader.cancel().catch(() => {});
  await until(async () => (await state()).leases === 0, 'cancelled HTTP stream releases lease');
  const afterCancel = await (await post()).json();
  assert.equal(afterCancel.active, 0);
  assert.equal(afterCancel.cancelled, 1, 'cancellation reaches container socket');
  pass('SSE bytes, EOF, cancellation propagation and active-stream idle protection');

  await action('idle');
  await until(async () => (await state()).state.status === 'stopped', 'idle shutdown');
  const restart = await post();
  assert.equal(restart.status, 200);
  assert.notEqual((await restart.json()).boot, first.boot, 'a fresh Docker process started');
  assert.equal((await post(undefined, { 'idempotency-key': replay })).status, 409);
  pass('idle shutdown, fresh-process wake and replay ledger survives container restart');
  await action('stop');
  await until(async () => (await state()).state.status === 'stopped', 'stop before readiness failure');
  const unready = await fetch(control + '/unready', { method: 'POST', signal: AbortSignal.timeout(5000) });
  assert.equal(unready.status, 200);
  assert.equal((await unready.json()).status, 503, 'production handler must bound stalled SDK startup');
  assert.equal((await state()).leases, 0, 'failed startup must release the request lease');
  await action('stop');
  await until(async () => {
    const response = await fetch(control + '/status');
    return response.ok && (await response.json()).state.status === 'stopped';
  }, 'failed readiness cleanup');
  pass('bounded SDK readiness failure and shutdown');
  console.log(`RESULT ${checks}/${checks} local HTTP groups passed`);
} finally {
  try { await action('stop'); }
  catch { console.error('Local container cleanup failed; stop wrangler-inference.'); process.exitCode = 1; }
}
