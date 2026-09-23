// Foreground supervision for local hosts without Amp orb services.
import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, open } from 'node:fs/promises';
import { createServer } from 'node:net';
import { resolve } from 'node:path';

const root = resolve(import.meta.dirname, '../..');
// Refuse collisions before preparing credentials or starting any process.
for (const port of [8791, 8792, 8793, 9291, 9292, 9293]) {
  await new Promise((accept, reject) => {
    const server = createServer();
    server.once('error', reject);
    server.listen(port, '127.0.0.1', () => server.close(accept));
  });
}
await import('./prepare.mjs');
const runtime = await mkdtemp(resolve(root, '.wrangler/local-e2e/process-'));
const home = resolve(runtime, 'home');
await mkdir(home);
const env = {
  PATH: process.env.PATH, HOME: home,
  XDG_CONFIG_HOME: resolve(home, 'config'), XDG_CACHE_HOME: resolve(home, 'cache'),
  XDG_DATA_HOME: resolve(home, 'data'), TMPDIR: process.env.TMPDIR || runtime,
  WRANGLER_SEND_METRICS: 'false',
  DOCKER_HOST: process.env.DOCKER_HOST || 'unix:///var/run/docker.sock',
};
// Rootless container engines require their caller-owned runtime directory.
if (process.env.XDG_RUNTIME_DIR) env.XDG_RUNTIME_DIR = process.env.XDG_RUNTIME_DIR;
const children = [];
let stopping = false;
async function stop(code) {
  if (stopping) return;
  stopping = true;
  process.exitCode = code;
  for (const { child } of children) {
    try { process.kill(-child.pid, 'SIGTERM'); } catch (error) { if (error.code !== 'ESRCH') throw error; }
  }
  const timer = setTimeout(() => {
    for (const { child } of children) {
      try { process.kill(-child.pid, 'SIGKILL'); } catch (error) { if (error.code !== 'ESRCH') throw error; }
    }
  }, 10000);
  await Promise.all(children.map(({ exited }) => exited));
  clearTimeout(timer);
}
process.once('SIGINT', () => void stop(130));
process.once('SIGTERM', () => void stop(143));
for (const name of ['inference', 'admin', 'control']) {
  const log = await open(resolve(runtime, `${name}.log`), 'a');
  if (stopping) { await log.close(); break; }
  const child = spawn(process.execPath, [resolve(root, 'node_modules/wrangler/bin/wrangler.js'),
    'dev', '--local', '--config', resolve(root, `.wrangler/local-e2e/${name}.json`),
    '--persist-to', resolve(runtime, `${name}-state`), '--show-interactive-dev-session=false'],
  { env, detached: true, stdio: ['ignore', log.fd, log.fd] });
  const exited = new Promise(accept => child.once('exit', (code, signal) => {
    accept();
    if (!stopping) { console.error(`${name} exited unexpectedly: ${code ?? signal}; logs: ${runtime}`); void stop(1); }
  }));
  child.once('error', error => { console.error(error); void stop(1); });
  children.push({ child, exited });
  await log.close();
}
console.log(`Stock local processes started; logs and state: ${runtime}`);
for (let attempt = 0; attempt < 60 && !stopping; attempt++) {
  try {
    const response = await fetch('http://127.0.0.1:8793/status', { signal: AbortSignal.timeout(1000) });
    if (response.ok) {
      console.log('Local Wrangler bindings ready. Keep this supervisor running; SIGINT/SIGTERM stops its children.');
      break;
    }
  } catch { /* Wait for local bindings. */ }
  if (attempt === 59) { console.error(`Local startup failed; logs: ${runtime}`); await stop(1); }
  else await new Promise(accept => setTimeout(accept, 1000));
}
