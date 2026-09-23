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

test('Nix gateway loads its native plugin, serves UI, enforces auth and stops', { timeout: 60000 }, async t => {
  assert.ok(process.env.BIFROST_PACKAGE, 'BIFROST_PACKAGE must name the built Nix output');
  const dir = await mkdtemp(join(tmpdir(), 'bifrost-package-'));
  const listener = net.createServer().listen(0, '127.0.0.1');
  await once(listener, 'listening');
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  await writeFile(join(dir, 'config.json'), JSON.stringify({
    encryption_key: 'a'.repeat(64),
    client: { enforce_auth_on_inference: true, enable_logging: false },
    config_store: { enabled: true, type: 'sqlite', config: { path: join(dir, 'config.db') } },
    providers: { openai: {
      keys: [{ id: 'synthetic', value: 'fixture-only', models: ['*'], weight: 1 }],
      network_config: { base_url: 'http://127.0.0.1:1', allow_private_network: true },
    } },
    governance: { auth_config: { is_enabled: true, admin_username: 'fixture', admin_password: 'synthetic-password-for-local-test-only' } },
    plugins: [
      { name: 'telemetry', enabled: false },
      { name: 'headroom', enabled: true, path: join(process.env.BIFROST_PACKAGE, 'lib/headroom.so'), placement: 'post_builtin', config: { enabled: false, ccr: false } },
    ],
  }), { mode: 0o600 });
  const child = spawn(join(process.env.BIFROST_PACKAGE, 'bin/bifrost-http'), [
    '-app-dir', dir, '-host', '127.0.0.1', '-port', String(port), '-log-level', 'error',
  ], { env: { PATH: process.env.PATH, HOME: dir }, stdio: 'ignore' });
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
  child.kill('SIGTERM');
  const [code, signal] = await exited;
  assert.equal(signal, null);
  assert.equal(code, 0);
});
