// BIFROST_PACKAGE=$(nix build .#bifrost-stack --no-link --print-out-paths) node --test deploy/nixos/package.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { setTimeout as delay } from 'node:timers/promises';
import net from 'node:net';
import http from 'node:http';

test('Nix gateway loads its native plugin, serves UI, enforces auth and stops', { timeout: 60000 }, async t => {
  assert.ok(process.env.BIFROST_PACKAGE, 'BIFROST_PACKAGE must name the built Nix output');
  const dir = await mkdtemp(join(tmpdir(), 'bifrost-package-'));
  const listener = net.createServer().listen(0, '127.0.0.1');
  await once(listener, 'listening');
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  const semantic = process.env.BIFROST_TEST_SEMANTIC === '1';
  const monitorListener = net.createServer().listen(0, '127.0.0.1');
  await once(monitorListener, 'listening');
  const monitorPort = monitorListener.address().port;
  await new Promise(resolve => monitorListener.close(resolve));
  let upstreamCalls = 0;
  let embeddingCalls = 0;
  let embeddingUnavailable = false;
  const upstream = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST' && req.url === '/v1/embeddings') {
      embeddingCalls++;
      assert.equal(req.headers['x-headroom-proxy-token'], 's'.repeat(32));
      assert.equal(req.headers.authorization, undefined);
      const body = JSON.parse(Buffer.concat(chunks));
      assert.equal(body.model, 'headroom-minilm-v1');
      if (embeddingUnavailable) {
        res.writeHead(503).end(JSON.stringify({ error: 'private upstream diagnostics' }));
        return;
      }
      res.end(JSON.stringify({ object: 'list', model: body.model,
        data: [{ index: 0, object: 'embedding', embedding: [1, ...Array(383).fill(0)] }] }));
      return;
    }
    if (req.method === 'GET' && req.url.endsWith('/models')) {
      res.end(JSON.stringify({ object: 'list', data: [{ id: 'synthetic', object: 'model', owned_by: 'fixture' }] }));
      return;
    }
    if (req.method !== 'POST' || !req.url.endsWith('/chat/completions')) {
      res.writeHead(404).end('{}');
      return;
    }
    upstreamCalls++;
    res.end(JSON.stringify({ id: `fixture-${upstreamCalls}`, object: 'chat.completion', model: 'synthetic',
      choices: [{ index: 0, message: { role: 'assistant', content: `response-${upstreamCalls}` }, finish_reason: 'stop' }],
      usage: { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 } }));
  }).listen(0, '127.0.0.1');
  await once(upstream, 'listening');
  t.after(() => new Promise(resolve => upstream.close(resolve)));
  const cache = process.env.VALKEY_TEST_ADDR ? [{ name: 'semantic_cache', enabled: true,
    config: { provider: semantic ? 'headroom_embeddings' : '', dimension: semantic ? 384 : 1,
      embedding_model: 'headroom-minilm-v1', threshold: 0.98, default_cache_key: 'package-test', scope_by_virtual_key: true,
      vector_store_namespace: `PackageCache${port}`, ttl: '1m' } }] : [];
  await writeFile(join(dir, 'config.json'), JSON.stringify({
    encryption_key: 'a'.repeat(64),
    client: { enforce_auth_on_inference: true, enable_logging: false },
    config_store: { enabled: true, type: 'sqlite', config: { path: join(dir, 'config.db') } },
    providers: { openai: {
      keys: [{ id: 'synthetic', value: 'fixture-only', models: ['synthetic'], weight: 1 }],
      network_config: { base_url: `http://127.0.0.1:${upstream.address().port}`, allow_private_network: true },
    }, ...(semantic ? { headroom_embeddings: {
      keys: [{ name: 'internal', value: 'env.HEADROOM_METRICS_TOKEN', models: ['headroom-minilm-v1'], weight: 1 }],
      custom_provider_config: { base_provider_type: 'openai', is_key_less: false, allowed_requests: { embedding: true } },
      network_config: { base_url: `http://127.0.0.1:${monitorPort}`, allow_private_network: true, max_retries: 0 },
    } } : {}) },
    vector_store: process.env.VALKEY_TEST_ADDR ? { enabled: true, type: 'redis',
      config: { addr: process.env.VALKEY_TEST_ADDR, password: 'synthetic-local-only' } } : undefined,
    governance: { auth_config: { is_enabled: true, admin_username: 'fixture', admin_password: 'synthetic-password-for-local-test-only' },
      virtual_keys: ['a', 'b'].map(id => ({ id: `tenant-${id}`, name: `tenant-${id}`, value: `sk-bf-fixture-${id}`, is_active: true,
        provider_configs: [{ provider: 'openai', allowed_models: ['synthetic'], key_ids: ['synthetic'], weight: 1 }] })) },
    plugins: [
      { name: 'telemetry', enabled: false },
      ...cache,
      { name: 'headroom', enabled: true, path: join(process.env.BIFROST_PACKAGE, 'lib/headroom.so'), placement: 'post_builtin', config: {
        enabled: semantic, ccr: false, endpoint: `http://127.0.0.1:${upstream.address().port}`,
        token_env: 'HEADROOM_PROXY_TOKEN', scope_key_env: 'HEADROOM_SCOPE_KEY',
        metrics_token_env: 'HEADROOM_METRICS_TOKEN', metrics_address: semantic ? `127.0.0.1:${monitorPort}` : '',
      } },
    ],
  }), { mode: 0o600 });
  const child = spawn(join(process.env.BIFROST_PACKAGE, 'bin/bifrost-http'), [
    '-app-dir', dir, '-host', '127.0.0.1', '-port', String(port), '-log-level', 'error',
  ], { env: { PATH: process.env.PATH, HOME: dir, HEADROOM_PROXY_TOKEN: 's'.repeat(32),
    HEADROOM_SCOPE_KEY: 'k'.repeat(32), HEADROOM_METRICS_TOKEN: 'm'.repeat(32) }, stdio: 'ignore' });
  const exited = once(child, 'exit');
  t.after(async () => {
    if (child.exitCode === null) child.kill('SIGTERM');
    await exited;
    await rm(dir, { recursive: true, force: true });
  });
  const origin = `http://127.0.0.1:${port}`;
  const headers = { authorization: `Basic ${Buffer.from('fixture:synthetic-password-for-local-test-only').toString('base64')}` };
  let ready = false;
  for (let i = 0; i < 150; i++) {
    assert.equal(child.exitCode, null, 'gateway exited before readiness');
    try {
      const response = await fetch(origin + '/api/plugins/loaded', { headers });
      if (response.status === 200) {
        assert.ok((await response.json()).plugins.includes('headroom'), 'native plugin did not load');
        ready = true;
        break;
      }
    } catch { /* readiness retry */ }
    await delay(100);
  }
  assert.equal(ready, true, 'gateway/plugin not ready');
  assert.equal((await fetch(origin + '/api/config')).status, 401);
  const rejected = await fetch(origin + '/v1/responses', {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: '{"model":"openai/synthetic","input":"test"}',
  });
  assert.ok([401, 403].includes(rejected.status), `unauthenticated inference: ${rejected.status} ${await rejected.text()}`);
  const html = await (await fetch(origin + '/login')).text();
  const asset = html.match(/src="(\/assets\/[^" ]+\.js)"/);
  assert.ok(asset, 'built UI entry missing');
  assert.equal((await fetch(origin + asset[1])).status, 200);
  await t.test('authenticated tenants cannot read each other’s cache', { skip: !process.env.VALKEY_TEST_ADDR }, async () => {
    const ask = async (key, text = 'synthetic cache isolation') => {
      const response = await fetch(origin + '/v1/chat/completions', { method: 'POST',
        headers: { 'content-type': 'application/json', 'x-bf-vk': key, 'x-bf-cache-key': 'same-caller-key' },
        body: JSON.stringify({ model: 'openai/synthetic', messages: [{ role: 'user', content: text }] }),
      });
      assert.equal(response.status, 200, response.status === 200 ? undefined : await response.text());
      return response.json();
    };
    const first = await ask('sk-bf-fixture-a');
    assert.equal(first.extra_fields.cache_debug.cache_hit, false);
    await delay(250);
    const repeated = await ask('sk-bf-fixture-a');
    assert.equal(repeated.extra_fields.cache_debug.cache_hit, true);
    assert.equal(repeated.choices[0].message.content, first.choices[0].message.content);
    const other = await ask('sk-bf-fixture-b');
    assert.equal(other.extra_fields.cache_debug.cache_hit, false);
    assert.notEqual(other.choices[0].message.content, first.choices[0].message.content);
    assert.equal(upstreamCalls, 2, 'only the two tenant misses should reach the provider');
    if (semantic) {
      const similar = await ask('sk-bf-fixture-a', 'a differently worded synthetic question');
      assert.equal(similar.extra_fields.cache_debug.cache_hit, true);
      assert.equal(similar.choices[0].message.content, first.choices[0].message.content);
      assert.equal(upstreamCalls, 2, 'semantic hit must not call the model');
      assert.ok(embeddingCalls >= 3, 'the real cache must use the authenticated embedding bridge');
      embeddingUnavailable = true;
      const uncached = await ask('sk-bf-fixture-a', 'embedding outage must not break inference');
      assert.equal(uncached.choices[0].message.content, 'response-3');
      assert.equal(upstreamCalls, 3);
      assert.ok(!JSON.stringify(uncached).includes('private upstream diagnostics'));
    }
  });
  child.kill('SIGTERM');
  const [code, signal] = await exited;
  assert.equal(signal, null);
  assert.equal(code, 0);
});
