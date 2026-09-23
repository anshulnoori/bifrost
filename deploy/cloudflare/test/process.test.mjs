import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { copyFile, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { resolve } from 'node:path';

test('process supervisor isolates children, rejects collisions and handles termination', async t => {
  const root = await mkdtemp(resolve(tmpdir(), 'bifrost-supervisor-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(resolve(root, 'test/local'), { recursive: true });
  await mkdir(resolve(root, 'node_modules/wrangler/bin'), { recursive: true });
  await copyFile(new URL('local/process.mjs', import.meta.url), resolve(root, 'test/local/process.mjs'));
  await writeFile(resolve(root, 'test/local/prepare.mjs'), `
    import { mkdir } from 'node:fs/promises';
    await mkdir(${JSON.stringify(resolve(root, '.wrangler/local-e2e'))}, { recursive: true });
  `);
  await writeFile(resolve(root, 'node_modules/wrangler/bin/wrangler.js'), `
    const http = require('node:http');
    const name = require('node:path').basename(process.argv[process.argv.indexOf('--config') + 1], '.json');
    console.log(JSON.stringify({ pid: process.pid, env: process.env, args: process.argv }));
    if (name === 'control') http.createServer((req, res) => res.end('{}')).listen(8793, '127.0.0.1');
    else setInterval(() => {}, 1000);
    process.on('SIGTERM', () => { console.log('terminated'); process.exit(0); });
  `);
  const launch = () => {
    const child = spawn(process.execPath, [resolve(root, 'test/local/process.mjs')], {
      env: { ...process.env, CLOUDFLARE_API_TOKEN: 'synthetic-must-not-inherit' }, stdio: ['ignore', 'pipe', 'pipe'],
    });
    let output = '';
    child.stdout.on('data', chunk => { output += chunk; });
    child.stderr.on('data', chunk => { output += chunk; });
    return { child, exited: once(child, 'exit'), output: () => output };
  };
  const wait = async predicate => {
    for (let i = 0; i < 100; i++) { if (await predicate()) return; await new Promise(r => setTimeout(r, 50)); }
    assert.fail('supervisor did not reach expected state');
  };
  await t.test('port collision fails before prepare', async () => {
    const blocker = createServer().listen(8791, '127.0.0.1');
    await once(blocker, 'listening');
    try {
      const run = launch();
      assert.deepEqual(await run.exited, [1, null]);
      assert.match(run.output(), /EADDRINUSE/);
      await assert.rejects(readdir(resolve(root, '.wrangler')), { code: 'ENOENT' });
    } finally { await new Promise(r => blocker.close(r)); }
  });
  for (const signal of ['SIGTERM', 'SIGINT', 'child']) await t.test(`${signal} stops all children`, async () => {
    const run = launch();
    try {
      await wait(() => run.output().includes('bindings ready'));
      const runtime = run.output().match(/logs and state: (.+)/)[1];
      const facts = {};
      for (const name of ['inference', 'admin', 'control']) {
        facts[name] = JSON.parse((await readFile(resolve(runtime, `${name}.log`), 'utf8')).split('\n')[0]);
        assert.equal(facts[name].env.CLOUDFLARE_API_TOKEN, undefined);
        assert.equal(facts[name].env.WRANGLER_SEND_METRICS, 'false');
        assert.equal(facts[name].env.HOME, resolve(runtime, 'home'));
        assert.ok(facts[name].args.includes('--local'));
        assert.ok(!facts[name].args.includes('--remote'));
      }
      if (signal === 'child') process.kill(facts.inference.pid, 'SIGTERM');
      else run.child.kill(signal);
      assert.deepEqual(await run.exited, [signal === 'child' ? 1 : signal === 'SIGINT' ? 130 : 143, null]);
      for (const name of ['inference', 'admin', 'control']) {
        assert.match(await readFile(resolve(runtime, `${name}.log`), 'utf8'), /terminated/);
        assert.throws(() => process.kill(facts[name].pid, 0), { code: 'ESRCH' });
      }
    } finally { if (run.child.exitCode === null) { run.child.kill('SIGTERM'); await run.exited; } }
  });
});
