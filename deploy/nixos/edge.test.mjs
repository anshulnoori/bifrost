// Real Caddy processes; synthetic upstream credentials only. No Tailscale login.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { setTimeout as delay } from 'node:timers/promises';

test('Funnel target is inference-only; private target retains session routes', { timeout: 15000 }, async t => {
  const seen = [];
  let cancelled = false;
  const upstream = http.createServer(async (req, res) => {
    seen.push({ path: req.url, headers: req.headers });
    if (req.url.startsWith('/api/') || req.url.startsWith('/login')) {
      res.end('private fixture');
      return;
    }
    if (req.headers['x-bf-vk'] !== 'sk-bf-synthetic') {
      res.writeHead(401).end();
      return;
    }
    const chunks = [];
    try { for await (const chunk of req) chunks.push(chunk); } catch { return; }
    const body = Buffer.concat(chunks).toString();
    if (body === '{"stream":true}') {
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      res.write('data: first\n\n');
      const timer = setInterval(() => res.write(': keepalive\n\n'), 25);
      res.on('close', () => { cancelled = true; clearInterval(timer); });
    } else {
      res.setHeader('content-type', 'application/json');
      res.end(JSON.stringify({ path: req.url, body }));
    }
  });
  upstream.listen(0, '127.0.0.1');
  await once(upstream, 'listening');
  // Acquire unused ports before starting Caddy; collision fails the test.
  const ports = [];
  for (let i = 0; i < 2; i++) {
    const server = http.createServer().listen(0, '127.0.0.1');
    await once(server, 'listening');
    ports.push(server.address().port);
    await new Promise(resolve => server.close(resolve));
  }
  // Model the Service hop back into the private Caddy listener.
  const serviceRequests = [];
  const service = http.createServer((req, res) => {
    serviceRequests.push(req.url);
    const forwarded = http.request({ hostname: '127.0.0.1', port: ports[1], path: req.url, method: req.method, headers: req.headers }, reply => {
      res.writeHead(reply.statusCode, reply.headers);
      reply.pipe(res);
    });
    forwarded.on('error', () => res.destroy());
    res.on('close', () => forwarded.destroy());
    req.pipe(forwarded);
  }).listen(0, '127.0.0.1');
  await once(service, 'listening');
  const caddy = spawn(process.env.CADDY ?? 'caddy', ['run', '--config', 'deploy/nixos/Caddyfile', '--adapter', 'caddyfile'], {
    env: { ...process.env, BIFROST_PORT: String(upstream.address().port), INFERENCE_PORT: String(ports[0]), ADMIN_PORT: String(ports[1]), INFERENCE_SERVICE_URL: `http://127.0.0.1:${service.address().port}` },
    stdio: process.env.CADDY_TEST_DEBUG ? 'inherit' : 'ignore',
  });
  const exited = once(caddy, 'exit');
  t.after(async () => {
    caddy.kill('SIGTERM');
    await exited;
    service.closeAllConnections();
    await new Promise(resolve => service.close(resolve));
    upstream.closeAllConnections();
    await new Promise(resolve => upstream.close(resolve));
  });
  const origin = `http://127.0.0.1:${ports[0]}`;
  for (let i = 0; ; i++) {
    try { await fetch(origin); break; } catch {
      if (i === 100 || caddy.exitCode !== null) throw new Error('Caddy did not become ready');
      await delay(50);
    }
  }
  const request = (path, headers = {}, body = '{}', method = 'POST') => fetch(origin + path, {
    method, headers: { 'content-type': 'application/json', authorization: 'Bearer sk-bf-synthetic', ...headers },
    body: method === 'GET' ? undefined : body,
  });

  await t.test('rejects admin, metrics, unknown paths, methods and encoded/query variants before upstream', async () => {
    const before = seen.length;
    for (const path of ['/api/config', '/api/session/oidc/callback?code=synthetic', '/health', '/metrics', '/v1/models', '/v1/compress', '/v1/responses/abc', '/v1/responses?x=1', '/v1/%72esponses', '//v1/responses', '/login']) {
      assert.equal((await request(path)).status, 404, path);
    }
    assert.equal((await request('/v1/responses', {}, '', 'GET')).status, 404);
    assert.equal(seen.length, before);
    assert.equal(serviceRequests.length, 0, 'admin paths must not reach the Service');
  });
  await t.test('accepts only VK-shaped credentials; actual authentication belongs to Bifrost', async () => {
    const before = seen.length;
    for (const authorization of ['', 'Bearer admin-password', 'Bearer sk-provider']) {
      assert.equal((await request('/v1/responses', { authorization })).status, 401);
    }
    assert.equal(seen.length, before);
    assert.equal((await request('/v1/responses', { authorization: 'Bearer sk-bf-invalid' })).status, 401);
    assert.equal((await request('/v1/responses', { 'x-bf-vk': 'sk-bf-synthetic' })).status, 401);
    for (const headers of [{}, { authorization: '', 'x-api-key': 'sk-bf-synthetic' }, { authorization: '', 'x-bf-vk': 'sk-bf-synthetic' }]) {
      assert.equal((await request('/v1/responses', headers)).status, 200);
    }
    // Funnel preserves the public host; listeners must not require Host: localhost.
    assert.equal((await request('/v1/chat/completions', { host: 'bifrost.example.ts.net' })).status, 200);
    assert.equal(serviceRequests.at(-1), '/v1/chat/completions', 'public inference must traverse the Service');
  });
  await t.test('preserves body bytes, maps Anthropic, strips cookies, identity and routing overrides', async () => {
    const body = '{ "model": "synthetic", "messages": [] }';
    const res = await request('/v1/messages', {
      cookie: 'token=synthetic-admin', 'x-bf-api-key': 'provider-secret', 'cf-access-jwt-assertion': 'forged',
      'tailscale-user-login': 'forged', 'x-bf-url': 'http://evil.invalid',
    }, body);
    assert.equal(res.status, 200);
    assert.deepEqual(await res.json(), { path: '/anthropic/v1/messages', body });
    const { headers } = seen.at(-1);
    for (const name of ['cookie', 'x-bf-api-key', 'cf-access-jwt-assertion', 'tailscale-user-login', 'x-bf-url', 'authorization']) {
      assert.equal(headers[name], undefined, name);
    }
    assert.equal(headers['x-bf-vk'], 'sk-bf-synthetic');
  });
  await t.test('rejects content encoding and oversized bodies', async () => {
    assert.equal((await request('/v1/responses', { 'content-encoding': 'gzip' })).status, 415);
    assert.equal((await request('/v1/responses', { 'content-type': 'text/plain' })).status, 415);
    const boundary = await request('/v1/responses', {}, 'x'.repeat(4 * 1024 * 1024));
    assert.equal(boundary.status, 200);
    assert.equal((await boundary.json()).body.length, 4 * 1024 * 1024);
    assert.equal((await request('/v1/responses', {}, 'x'.repeat(4 * 1024 * 1024 + 1))).status, 413);
  });
  await t.test('SSE arrives before completion and disconnect cancels upstream', async () => {
    const res = await request('/v1/responses', {}, '{"stream":true}');
    const reader = res.body.getReader();
    const { value } = await reader.read();
    assert.match(new TextDecoder().decode(value), /^data: first\n\n/);
    await reader.cancel();
    for (let i = 0; i < 100 && !cancelled; i++) await delay(20);
    assert.equal(cancelled, true);
  });
  await t.test('private target enforces inference policy without accepting session cookies as API keys', async () => {
    const endpoint = `http://127.0.0.1:${ports[1]}/v1/chat/completions`;
    const denied = await fetch(endpoint, {
      method: 'POST', headers: { 'content-type': 'application/json', cookie: 'token=synthetic-admin' }, body: '{}',
    });
    assert.equal(denied.status, 401);
    const accepted = await fetch(endpoint, {
      method: 'POST', headers: { 'content-type': 'application/json', authorization: 'Bearer sk-bf-synthetic', cookie: 'token=synthetic-admin' }, body: '{}',
    });
    assert.equal(accepted.status, 200);
    assert.equal(seen.at(-1).headers.cookie, undefined);
    assert.equal(seen.at(-1).headers['x-bf-vk'], 'sk-bf-synthetic');
    assert.equal((await fetch(`http://127.0.0.1:${ports[1]}/v1/models`)).status, 404);
  });
  await t.test('separate private target forwards OIDC callback and session cookie', async () => {
    const response = await fetch(`http://127.0.0.1:${ports[1]}/api/session/oidc/callback?code=synthetic`, {
      headers: { cookie: '__Host-bifrost_oidc=synthetic', 'tailscale-user-login': 'forged', 'cf-access-jwt-assertion': 'forged', 'x-forwarded-proto': 'http', 'x-forwarded-custom': 'forged' },
    });
    assert.equal(await response.text(), 'private fixture');
    assert.equal(seen.at(-1).headers.cookie, '__Host-bifrost_oidc=synthetic');
    assert.equal(seen.at(-1).headers['tailscale-user-login'], undefined);
    assert.equal(seen.at(-1).headers['cf-access-jwt-assertion'], undefined);
    assert.equal(seen.at(-1).headers['x-forwarded-custom'], undefined);
    assert.equal(seen.at(-1).headers['x-forwarded-proto'], 'https');
  });
});
