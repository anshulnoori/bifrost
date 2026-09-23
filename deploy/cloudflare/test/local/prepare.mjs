import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { generateKeyPair, exportJWK, exportPKCS8 } from 'jose';
import { createHash } from 'node:crypto';

// Generate fresh synthetic credentials in ignored scratch, never cloud secrets.
const root = resolve(import.meta.dirname, '../..');
const dir = resolve(root, '.wrangler/local-e2e');
await mkdir(dir, { recursive: true });
const { publicKey, privateKey } = await generateKeyPair('RS256', { extractable: true });
const jwk = { ...await exportJWK(publicKey), kid: 'local-fixture', alg: 'RS256', use: 'sig' };
await writeFile(resolve(dir, 'signing-key.pem'), await exportPKCS8(privateKey), { mode: 0o600 });
await writeFile(resolve(dir, 'jose-local.mjs'), `
import { createLocalJWKSet } from ${JSON.stringify(resolve(root, 'node_modules/jose/dist/webapi/index.js'))};
export { jwtVerify } from ${JSON.stringify(resolve(root, 'node_modules/jose/dist/webapi/index.js'))};
export function createRemoteJWKSet() { return createLocalJWKSet(${JSON.stringify({ keys: [jwk] })}); }
`);
const parse = async path => JSON.parse(await readFile(resolve(root, path), 'utf8'));
const inference = await parse('inference-worker/wrangler.jsonc');
const admin = await parse('admin-worker/wrangler.jsonc');
const registry = {};
for (const [name, rpm] of [['normal', 120], ['limited', 2]]) {
  const key = `sk-bf-fixture-${name}-` + 'x'.repeat(32);
  registry[createHash('sha256').update(key).digest('hex')] = { expires: Math.floor(Date.now() / 1000) + 3600, models: ['fixture/model'], rpm, daily_requests: 1000 };
}
const identity = { ACCESS_ISSUER: 'https://fixture.cloudflareaccess.com', ACCESS_AUD: 'fixture-audience', OWNER_EMAIL: 'owner@example.test', OWNER_SUB: 'fixture-owner', ADMIN_ORIGIN: 'http://127.0.0.1:8792' };
inference.name = 'bifrost-inference-local';
inference.main = resolve(root, 'test/local/entry.ts');
inference.vars = { ...inference.vars, ...identity, INFERENCE_ORIGIN: 'http://127.0.0.1:8791', EMERGENCY_DISABLE: 'false', INFERENCE_KEYS_JSON: JSON.stringify(registry) };
for (const name of ['NEON_DATABASE_URL', 'BIFROST_ENCRYPTION_KEY', 'BIFROST_ADMIN_PASSWORD', 'HEADROOM_ENDPOINT', 'HEADROOM_PROXY_TOKEN', 'HEADROOM_SCOPE_KEY', 'HEADROOM_METRICS_TOKEN', 'MODAL_TOKEN_ID', 'MODAL_TOKEN_SECRET']) inference.vars[name] = `synthetic-${name.toLowerCase()}-not-a-real-credential`;
inference.containers[0].image = resolve(root, 'test/local/Dockerfile');
inference.containers[0].image_build_context = resolve(root, 'test/local');
inference.alias = { jose: resolve(dir, 'jose-local.mjs') };
inference.dev = { ip: '127.0.0.1', port: 8791, inspector_port: 9291, enable_containers: true };
admin.name = 'bifrost-admin-local';
admin.main = resolve(root, 'admin-worker/index.ts');
admin.vars = identity;
admin.alias = inference.alias;
admin.services[0].service = inference.name;
admin.dev = { ip: '127.0.0.1', port: 8792, inspector_port: 9292 };
const control = { name: 'bifrost-control-local', main: resolve(root, 'test/local/control.ts'), compatibility_date: inference.compatibility_date,
  workers_dev: false, preview_urls: false, routes: [], dev: { ip: '127.0.0.1', port: 8793, inspector_port: 9293 },
  durable_objects: { bindings: [{ name: 'BIFROST', class_name: 'BifrostContainer', script_name: inference.name }] } };
for (const [name, config] of Object.entries({ inference, admin, control })) await writeFile(resolve(dir, `${name}.json`), JSON.stringify(config, null, 2));
console.log('Prepared local-only fixture configs and fresh signing key; no cloud access.');
