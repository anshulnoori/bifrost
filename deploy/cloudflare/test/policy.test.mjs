import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { generateKeyPair, SignJWT } from 'jose';
import { inference, admin, digest, readBody, MAX_BODY } from '../shared/policy.mjs';
import { admit, drainBody } from '../shared/lifecycle.mjs';

const key = 'sk-bf-' + 'fixture-not-a-real-key'.repeat(2);
const hash = await digest(key);
const policy = { expires: 4000000000, models: ['codex/test-model'], rpm: 2, daily_requests: 3 };
const env = { INFERENCE_ORIGIN: 'https://infer.example', ADMIN_ORIGIN: 'https://admin.example', EMERGENCY_DISABLE: 'false', INFERENCE_KEYS_JSON: JSON.stringify({ [hash]: policy }), ACCESS_ISSUER: 'https://test.cloudflareaccess.com', ACCESS_AUD: 'aud', OWNER_EMAIL: 'owner@example.test', OWNER_SUB: 'owner-sub' };
const req = (path = '/v1/responses', body = '{"model":"codex/test-model","input":"x"}', headers = {}, method = 'POST') => new Request(env.INFERENCE_ORIGIN + path, { method, headers: { authorization: `Bearer ${key}`, 'content-type': 'application/json', ...headers }, ...(method === 'POST' ? { body } : {}) });
const forbidden = ['/api/config', '/api/providers', '/api/codex/connections', '/api/plugins', '/api/headroom/events', '/metrics', '/health', '/health/details', '/v1/compress', '/status', '/wake', '/admin/stop', '/v1/models', '/v1/responses/id', '/v1/realtime', '/openai/v1/chat/completions', '/v1/messages/batches', '/v1/messages/x', '/v1/responses/', '/v1/%72esponses', '//v1/responses', '/unknown', '/debug/pprof'];

test('public allowlist rejects route scans before wake even with a valid key', async () => {
  let wakes = 0;
  for (const path of forbidden) for (const method of ['POST', 'GET', 'OPTIONS', 'DELETE']) {
    const res = await inference(req(path, '{}', {}, method), env, () => { wakes++; });
    assert.equal(res.status, 404, `${method} ${path}`);
  }
  assert.equal(wakes, 0);
});

test('invalid auth, expired/revoked keys, content and model restrictions never wake', async () => {
  let wakes = 0;
  const proxy = () => { wakes++; return new Response('bad'); };
  for (const [request, status, config] of [
    [req('/v1/responses', '{}', { authorization: '' }), 401],
    [req('/v1/responses', '{}', { 'x-api-key': key }), 401],
    [req('/v1/responses', '{}', { authorization: 'Bearer admin' }), 401],
    [req(), 401, { INFERENCE_KEYS_JSON: '{}' }],
    [req(), 401, { INFERENCE_KEYS_JSON: JSON.stringify({ [hash]: { ...policy, expires: 1 } }) }],
    [req(), 503, { EMERGENCY_DISABLE: 'true' }],
    [req('/v1/responses', 'not json'), 400],
    [req('/v1/responses', '{"model":"forbidden","model":"codex/test-model","input":"x"}'), 400],
    [req('/v1/responses', '{"model":"forbidden","mo\\u0064el":"codex/test-model","input":"x"}'), 400],
    [req('/v1/responses', '{"model":"other"}'), 403],
    [req('/v1/responses', '{"model":"codex/test-model","fallbacks":[]}'), 400],
    [req('/v1/responses?key=secret'), 404],
    [req('/v1/responses', '{}', { 'content-length': String(MAX_BODY + 1) }), 413],
    [req('/v1/responses', '{}', { 'content-type': 'text/plain' }), 415],
    [req('/v1/responses', '{}', { upgrade: 'websocket' }), 400],
  ]) assert.equal((await inference(request, { ...env, ...config }, proxy)).status, status);
  assert.equal(wakes, 0);
});

test('request body bytes and SSE preserved, spoofed headers stripped, Anthropic path fixed', async () => {
  const body = '{ "model": "codex/test-model", "messages": [], "stream": true }';
  const wire = 'event: delta\ndata: {"text":"hello"}\n\ndata: [DONE]\n\n';
  const response = await inference(req('/v1/messages', body, {
    'x-deployment-plane': 'admin', 'x-bf-vk': 'evil', 'x-bf-api-key': 'provider',
    'cf-access-jwt-assertion': 'spoofed', 'x-forwarded-proto': 'http', cookie: 'token=stolen',
    'x-bf-store-raw-request-response': 'true', 'x-container-port': '9909',
  }), env, async request => {
    assert.equal(new URL(request.url).pathname, '/anthropic/v1/messages');
    assert.equal(await request.text(), body);
    assert.equal(request.headers.get('x-bf-vk'), key);
    assert.equal(request.headers.get('x-deployment-plane'), 'inference');
    assert.equal(request.headers.get('x-forwarded-proto'), 'https');
    for (const name of ['cookie', 'cf-access-jwt-assertion', 'x-bf-api-key', 'x-bf-store-raw-request-response', 'x-container-port', 'authorization']) assert.equal(request.headers.get(name), null, name);
    return new Response(wire, { headers: { 'content-type': 'text/event-stream' } });
  });
  assert.equal(await response.text(), wire);
});

test('chunked body limit counts actual bytes', async () => {
  const request = new Request('https://x', { method: 'POST', body: new ReadableStream({ start(c) { c.enqueue(new Uint8Array(3)); c.enqueue(new Uint8Array(4)); c.close(); } }), duplex: 'half' });
  await assert.rejects(readBody(request, 6), /body limit/);
  assert.equal((await readBody(new Request('https://x', { method: 'POST', body: '123456' }), 6)).length, 6);
});

const pair = await generateKeyPair('RS256');
const token = async (overrides = {}) => new SignJWT({ email: env.OWNER_EMAIL, type: 'app', ...overrides }).setProtectedHeader({ alg: 'RS256' }).setIssuer(env.ACCESS_ISSUER).setAudience(env.ACCESS_AUD).setSubject(env.OWNER_SUB).setIssuedAt().setExpirationTime('10m').sign(pair.privateKey);
const adminReq = (path, jwt, headers = {}, method = 'GET') => new Request(env.ADMIN_ORIGIN + path, { method, headers: { 'cf-access-jwt-assertion': jwt, ...headers } });

test('admin requires cryptographic Access identity and never accepts inference credentials', async () => {
  let wakes = 0;
  const proxy = () => { wakes++; return new Response('ok'); };
  const jwt = await token();
  for (const [request, config] of [
    [adminReq('/api/providers', 'forged', { 'cf-access-authenticated-user-email': env.OWNER_EMAIL })],
    [adminReq('/api/providers', await token({ email: 'other@example.test' }))],
    [adminReq('/api/providers', jwt), { ACCESS_AUD: 'wrong' }],
    [adminReq('/api/providers', jwt), { ACCESS_ISSUER: 'https://wrong.cloudflareaccess.com' }],
    [adminReq('/api/providers', jwt), { OWNER_SUB: 'wrong' }],
    [adminReq('/api/providers', jwt, { 'x-bf-vk': key })],
    [adminReq('/api/providers', jwt, { authorization: `Bearer ${key}` })],
    [adminReq('/api/providers', jwt, { origin: 'https://evil.example' }, 'POST')],
  ]) assert.equal((await admin(request, { ...env, ...config }, proxy, pair.publicKey)).status, 403);
  assert.equal(wakes, 0);
  for (const path of ['/metrics', '/api/headroom/events', '/api/plugins', '/v1/responses', '/api/governance/virtual-keys/quota', '/health']) assert.equal((await admin(adminReq(path, jwt), env, proxy, pair.publicKey)).status, 404);
  assert.equal(wakes, 0);
  await admin(adminReq('/api/providers', jwt, { cookie: 'CF_Authorization=private; token=session-token', 'cf-access-authenticated-user-email': 'spoof', 'x-bf-api-key': 'secret' }), env, request => {
    assert.equal(request.headers.get('cookie'), 'token=session-token');
    for (const name of ['cf-access-jwt-assertion', 'cf-access-authenticated-user-email', 'x-bf-api-key']) assert.equal(request.headers.get(name), null);
    return new Response('ok');
  }, pair.publicKey);
});

test('expired Access JWT fails closed', async () => {
  const jwt = await new SignJWT({ email: env.OWNER_EMAIL, type: 'app' }).setProtectedHeader({ alg: 'RS256' }).setIssuer(env.ACCESS_ISSUER).setAudience(env.ACCESS_AUD).setSubject(env.OWNER_SUB).setIssuedAt(100).setExpirationTime(200).sign(pair.privateKey);
  assert.equal((await admin(adminReq('/', jwt), env, () => assert.fail('wake'), pair.publicKey)).status, 403);
});

test('OIDC admin flow preserves only browser binding and bounded callback parameters', async () => {
  const jwt = await token();
  const browser = 'a'.repeat(43);
  const cookie = `CF_Authorization=private; token=session-token; __Host-bifrost_oidc=${browser}; other=ignored`;
  for (const [path, method] of [
    ['/api/session/oidc/login', 'POST'],
    [`/api/session/oidc/callback?state=${browser}&code=synthetic-code`, 'GET'],
    [`/api/session/oidc/callback?state=${browser}&error=access_denied&error_description=cancelled`, 'GET'],
  ]) {
    assert.equal((await admin(adminReq(path, jwt, { cookie, origin: env.ADMIN_ORIGIN }, method), env, request => {
      assert.equal(new URL(request.url).pathname + new URL(request.url).search, path);
      assert.equal(request.headers.get('cookie'), `token=session-token; __Host-bifrost_oidc=${browser}`);
      assert.equal(request.headers.get('cf-access-jwt-assertion'), null);
      return new Response(null, { status: 303, headers: { location: '/login?oidc_error=cancelled', 'set-cookie': `__Host-bifrost_oidc=${browser}; Secure; HttpOnly; Path=/; SameSite=Lax` } });
    }, pair.publicKey)).status, 303);
    assert.equal((await admin(adminReq(path, 'forged', { cookie }, method), env, () => assert.fail('wake'), pair.publicKey)).status, 403);
    assert.equal((await inference(req(path, '{}', {}, method), env, () => assert.fail('public wake'))).status, 404);
  }
  assert.equal((await admin(adminReq('/api/session/oidc/login', jwt, { origin: 'https://evil.example' }, 'POST'), env, () => assert.fail('wake'), pair.publicKey)).status, 403);
  for (const error of ['cancelled', 'invalid', 'unavailable', 'denied']) {
    assert.equal((await admin(adminReq(`/login?oidc_error=${error}`, jwt), env, () => new Response('ok'), pair.publicKey)).status, 200);
  }
  for (const path of [
    '/api/session/oidc/login?code=x', '/login?oidc_error=unknown', '/login?oidc_error=invalid&oidc_error=denied',
    '/api/session/oidc/callback?state=a&state=b', '/api/session/oidc/callback?redirect_uri=https://evil.example',
    `/api/session/oidc/callback?code=${'x'.repeat(8193)}`,
  ]) assert.equal((await admin(adminReq(path, jwt, { origin: env.ADMIN_ORIGIN }, path.startsWith('/api/session/oidc/login') ? 'POST' : 'GET'), env, () => assert.fail('wake'), pair.publicKey)).status, 400);
  for (const [path, method] of [['/api/session/oidc/login', 'GET'], ['/api/session/oidc/callback', 'POST']]) {
    assert.equal((await admin(adminReq(path, jwt, { origin: env.ADMIN_ORIGIN }, method), env, () => assert.fail('wake'), pair.publicKey)).status, 404);
  }
  await admin(adminReq('/api/providers', jwt, { cookie }), env, request => {
    assert.equal(request.headers.get('cookie'), 'token=session-token');
    return new Response('ok');
  }, pair.publicKey);
});

test('restricted dashboard admits its bootstrap routes but not arbitrary config queries', async () => {
  const jwt = await token();
  for (const path of ['/login', '/workspace/providers', '/workspace/governance/virtual-keys', '/assets/index-fixture.js', '/bifrost-logo-dark.webp', '/api/auth/type', '/api/branding', '/api/config?from_db=false']) {
    assert.equal((await admin(adminReq(path, jwt), env, () => new Response('ok'), pair.publicKey)).status, 200, path);
    assert.equal((await admin(adminReq(path, 'forged'), env, () => assert.fail('wake'), pair.publicKey)).status, 403, path);
  }
  for (const path of ['/api/config?from_db=invalid', '/api/config?url=https://evil.example', '/api/providers?from_db=false']) {
    assert.equal((await admin(adminReq(path, jwt), env, () => assert.fail('wake'), pair.publicKey)).status, 400);
  }
});

test('admission limits keys, IPs, daily usage and idempotency replay', async () => {
  const values = new Map();
  const tx = { get: async k => values.get(k), put: async (k, v) => values.set(k, v) };
  const store = { transaction: fn => fn(tx) };
  const headers = new Headers({ 'x-deployment-key': hash, 'x-deployment-ip': 'a'.repeat(64), 'x-deployment-rpm': '2', 'x-deployment-daily': '3', 'x-deployment-replay': 'b'.repeat(64) });
  assert.equal(await admit(store, headers, 0), 200);
  assert.equal(await admit(store, headers, 1), 409);
  headers.delete('x-deployment-replay');
  assert.equal(await admit(store, headers, 1), 200);
  assert.equal(await admit(store, headers, 59999), 429);
  assert.equal(await admit(store, headers, 60000), 200);
  assert.equal(await admit(store, headers, 120000), 429);
  assert.equal(await admit(store, headers, 86400000), 200);
  headers.set('x-deployment-key', 'c'.repeat(64));
  assert.equal(await admit(store, headers, 86400001), 200);
});

test('stream lease releases only on EOF/cancel/error, cancellation reaches source', async () => {
  let finish = 0, cancel = 0, push;
  const source = new ReadableStream({ start(c) { push = c; }, cancel() { cancel++; } });
  const controller = new AbortController();
  const reader = drainBody(source, async () => { finish++; }, controller.signal).getReader();
  push.enqueue(new TextEncoder().encode('data: first\n\n'));
  assert.equal(new TextDecoder().decode((await reader.read()).value), 'data: first\n\n');
  assert.equal(finish, 0);
  controller.abort();
  await reader.read();
  await new Promise(r => setTimeout(r, 0));
  assert.equal(finish, 1);
  assert.equal(cancel, 1);
  assert.equal(await new Response(drainBody(new Response('last').body, async () => { finish++; })).text(), 'last');
  assert.equal(finish, 2);
});

test('shipping Workers enable incoming request cancellation without test aliases', async () => {
  for (const name of ['inference-worker', 'admin-worker']) {
    const config = JSON.parse(await readFile(new URL(`../${name}/wrangler.jsonc`, import.meta.url), 'utf8'));
    assert.ok(config.compatibility_flags.includes('enable_request_signal'));
    assert.equal(config.alias, undefined);
    assert.equal(config.main, 'index.ts');
  }
});

test('IP limit aggregates separate keys and resets at the window boundary', async () => {
  const values = new Map();
  const store = { transaction: fn => fn({ get: async k => values.get(k), put: async (k, v) => values.set(k, v) }) };
  const headers = new Headers({ 'x-deployment-ip': 'a'.repeat(64), 'x-deployment-rpm': '120', 'x-deployment-daily': '1000' });
  for (let i = 0; i < 180; i++) {
    headers.set('x-deployment-key', (i % 2 ? 'b' : 'c').repeat(64));
    assert.equal(await admit(store, headers, 0), 200);
  }
  assert.equal(await admit(store, headers, 59999), 429);
  assert.equal(await admit(store, headers, 60000), 200);
});
