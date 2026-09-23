// Key is read from a protected file, never argv, environment dumps, or output.
import { readFile, stat } from 'node:fs/promises';
import { randomBytes } from 'node:crypto';
import { canonical, signature } from '../src/auth.mjs';

const [origin, action, keyFile] = process.argv.slice(2);
if (!origin || !['wake', 'status'].includes(action) || !keyFile ||
    new URL(origin).origin !== origin || !origin.startsWith('https://')) {
  throw new Error('Usage: node scripts/control.mjs https://WAKE-HOST wake|status /private/path/wake.key');
}
if (((await stat(keyFile)).mode & 0o077) !== 0) throw new Error('Key file must be mode 0600');
const secret = (await readFile(keyFile, 'utf8')).trim();
if (!/^[a-f0-9]{64}$/.test(secret)) throw new Error('Key must contain 32 random bytes encoded as lowercase hex');
const time = String(Math.floor(Date.now() / 1000));
const nonce = randomBytes(16).toString('hex');
const response = await fetch(`${origin}/${action}`, {
  method: 'POST', redirect: 'error', signal: AbortSignal.timeout(60_000),
  headers: {
    'content-length': '0',
    'x-wake-time': time, 'x-wake-nonce': nonce,
    'x-wake-signature': await signature(secret, canonical(origin, `/${action}`, time, nonce)),
  },
});
// Only print expected lifecycle fields, never an arbitrary remote response body.
let state;
try { state = (await response.json()).status; } catch { state = 'unavailable'; }
if (!['running', 'stopped', 'rejected', 'unconfigured', 'unavailable'].includes(state)) state = 'unavailable';
console.log(JSON.stringify({ at: new Date().toISOString(), http: response.status, status: state }));
if (!response.ok) process.exitCode = 1;
