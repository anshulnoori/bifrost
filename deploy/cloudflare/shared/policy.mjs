import { createRemoteJWKSet, jwtVerify } from 'jose';

export const MAX_BODY = 4 * 1024 * 1024;
export const inferenceRoutes = new Map([
  ['/v1/chat/completions', '/v1/chat/completions'],
  ['/v1/responses', '/v1/responses'],
  ['/v1/messages', '/anthropic/v1/messages'],
]);

// Deliberately not a /api/* proxy. Plugin installation, logs, Headroom,
// diagnostics, lifecycle controls, passthrough and inference are absent.
const adminRoutes = [
  ['GET', /^\/(?:|login|workspace(?:\/(?:providers|virtual-keys|governance\/virtual-keys))?)$/],
  ['GET', /^\/(?:assets|images|static)\/[A-Za-z0-9_./-]+\.(?:js|css|png|webp|svg|ico|woff2)$/],
  ['GET', /^\/bifrost-logo(?:-dark)?\.webp$/],
  ['GET', /^\/favicon.ico$/],
  ['GET', /^\/api\/(?:version|config|auth\/type|branding|models|models\/details|models\/parameters|models\/base|keys|session\/is-auth-enabled)$/],
  ['POST', /^\/api\/session\/(?:login|logout)$/],
  ['POST', /^\/api\/session\/oidc\/login$/],
  ['GET', /^\/api\/session\/oidc\/callback$/],
  ['GET', /^\/api\/providers(?:\/[a-z0-9_-]+(?:\/keys(?:\/[a-zA-Z0-9_-]+)?)?)?$/],
  ['POST', /^\/api\/providers(?:\/[a-z0-9_-]+\/keys)?$/],
  ['PUT', /^\/api\/providers\/[a-z0-9_-]+(?:\/keys\/[a-zA-Z0-9_-]+)?$/],
  ['DELETE', /^\/api\/providers\/[a-z0-9_-]+(?:\/keys\/[a-zA-Z0-9_-]+)?$/],
  ['GET', /^\/api\/codex\/connections\/(?:current|usage)$/],
  ['POST', /^\/api\/codex\/connections(?:\/[a-zA-Z0-9_-]+\/poll)?$/],
  ['DELETE', /^\/api\/codex\/connections\/[a-zA-Z0-9_-]+$/],
  ['GET', /^\/api\/governance\/(?:budgets|rate-limits|providers)$/],
  ['GET', /^\/api\/governance\/virtual-keys(?:\/[a-zA-Z0-9_-]+)?$/],
  ['POST', /^\/api\/governance\/virtual-keys(?:\/[a-zA-Z0-9_-]+\/rotate)?$/],
  ['PUT', /^\/api\/governance\/virtual-keys\/[a-zA-Z0-9_-]+$/],
  ['DELETE', /^\/api\/governance\/virtual-keys\/[a-zA-Z0-9_-]+$/],
];

export function reply(status, id = crypto.randomUUID()) {
  return new Response(JSON.stringify({ error: 'request rejected', request_id: id }), {
    status, headers: { 'content-type': 'application/json', 'cache-control': 'no-store', 'x-request-id': id },
  });
}

export function cleanURL(request, origin) {
  const url = new URL(request.url);
  if (url.origin !== origin || url.username || url.password || url.hash || /[%\\\x00-\x20]/.test(url.pathname) || url.pathname.includes('//')) return null;
  return url;
}

export function adminAllowed(method, path) {
  if (path === '/api/governance/virtual-keys/quota') return false;
  return adminRoutes.some(([verb, pattern]) => method === verb && pattern.test(path));
}

export async function digest(value) {
  return Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value))), b => b.toString(16).padStart(2, '0')).join('');
}

function uniqueTopLevelKeys(text) {
  // JSON.parse validated syntax already. Tokenize strings as units so braces
  // and commas inside prompt text cannot change depth. Preserve original bytes.
  const keys = new Set();
  let depth = 0, keyExpected = false;
  for (const [token] of text.matchAll(/"(?:\\.|[^"\\])*"|[{}\[\],:]/g)) {
    if (token === '{' || token === '[') { depth++; keyExpected = depth === 1; }
    else if (token === '}' || token === ']') depth--;
    else if (token === ',' && depth === 1) keyExpected = true;
    else if (token.startsWith('"') && depth === 1 && keyExpected) {
      const name = JSON.parse(token);
      if (keys.has(name)) return false;
      keys.add(name);
      keyExpected = false;
    }
  }
  return true;
}

export async function readBody(request, limit = MAX_BODY) {
  const declared = request.headers.get('content-length');
  if (declared !== null && (!/^\d+$/.test(declared) || Number(declared) > limit)) throw new Error('body limit');
  if (!request.body) return new Uint8Array();
  const reader = request.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > limit) { await reader.cancel(); throw new Error('body limit'); }
      chunks.push(value);
    }
  } finally { reader.releaseLock(); }
  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { body.set(chunk, offset); offset += chunk.byteLength; }
  return body;
}

export function headersFor(request, plane, id) {
  const headers = new Headers();
  // Allowlist instead of an ever-growing denylist. No upstream URL, provider
  // key, routing override, port, Access identity or content-log override survives.
  const allowed = plane === 'inference'
    ? ['content-type', 'accept', 'anthropic-version', 'anthropic-beta', 'x-headroom-thread', 'x-bf-session-id']
    : ['content-type', 'accept', 'x-bf-codex-key', 'sec-fetch-site', 'origin'];
  for (const name of allowed) {
    const value = request.headers.get(name);
    if (value !== null && value.length <= 1024) headers.set(name, value);
  }
  if (plane === 'admin') {
    // Only the session and OIDC flow's browser binding. Never forward Access cookies.
    const cookies = request.headers.get('cookie')?.split(';').map(x => x.trim()) ?? [];
    const selected = [cookies.find(x => /^token=[A-Za-z0-9._~-]+$/.test(x))];
    if (['/api/session/oidc/login', '/api/session/oidc/callback'].includes(new URL(request.url).pathname)) {
      selected.push(cookies.find(x => /^__Host-bifrost_oidc=[A-Za-z0-9_-]{43}$/.test(x)));
    }
    if (selected.some(Boolean)) headers.set('cookie', selected.filter(Boolean).join('; '));
  }
  headers.set('x-forwarded-proto', 'https');
  headers.set('x-request-id', id);
  return headers;
}

const keysets = new Map();
export async function accessIdentity(request, env, keyset) {
  if (!/^https:\/\/[a-z0-9-]+\.cloudflareaccess\.com$/.test(env.ACCESS_ISSUER ?? '') || !env.ACCESS_AUD || !env.OWNER_EMAIL || !env.OWNER_SUB) throw new Error('unconfigured');
  if (!keyset) {
    if (!keysets.has(env.ACCESS_ISSUER)) keysets.set(env.ACCESS_ISSUER, createRemoteJWKSet(new URL(`${env.ACCESS_ISSUER}/cdn-cgi/access/certs`), { timeoutDuration: 3000 }));
    keyset = keysets.get(env.ACCESS_ISSUER);
  }
  const { payload } = await jwtVerify(request.headers.get('cf-access-jwt-assertion') ?? '', keyset, {
    issuer: env.ACCESS_ISSUER, audience: env.ACCESS_AUD, algorithms: ['RS256'],
    requiredClaims: ['exp', 'iat', 'sub', 'email'], maxTokenAge: '1h', clockTolerance: 5,
  });
  if (payload.email !== env.OWNER_EMAIL || payload.sub !== env.OWNER_SUB || payload.type !== 'app') throw new Error('identity');
  return payload;
}

export async function inference(request, env, proxy) {
  const id = crypto.randomUUID();
  const url = cleanURL(request, env.INFERENCE_ORIGIN);
  if (!url || url.search || request.method !== 'POST' || !inferenceRoutes.has(url.pathname)) return reply(404, id);
  if (env.EMERGENCY_DISABLE !== 'false') return reply(503, id);
  if (request.headers.has('upgrade') || request.headers.has('content-encoding')) return reply(400, id);
  const auth = request.headers.get('authorization');
  const apiKey = request.headers.get('x-api-key');
  if ((auth && apiKey) || (!auth && !apiKey)) return reply(401, id);
  const key = apiKey ?? (/^Bearer (sk-bf-[A-Za-z0-9_-]{20,200})$/.exec(auth ?? '')?.[1]);
  if (!key || !/^sk-bf-[A-Za-z0-9_-]{20,200}$/.test(key)) return reply(401, id);
  let policy, keyHash;
  try {
    keyHash = await digest(key);
    policy = JSON.parse(env.INFERENCE_KEYS_JSON ?? '{}')[keyHash];
    if (!policy || !Number.isSafeInteger(policy.expires) || policy.expires <= Date.now() / 1000 || !Array.isArray(policy.models) || !policy.models.length || !Number.isInteger(policy.rpm) || policy.rpm < 1 || policy.rpm > 120 || !Number.isInteger(policy.daily_requests) || policy.daily_requests < 1) return reply(401, id);
  } catch { return reply(503, id); }
  if (request.headers.get('content-type')?.split(';')[0].trim() !== 'application/json') return reply(415, id);
  let bytes, body, text;
  try { bytes = await readBody(request); } catch { return reply(413, id); }
  try { text = new TextDecoder('utf-8', { fatal: true }).decode(bytes); body = JSON.parse(text); } catch { return reply(400, id); }
  if (!uniqueTopLevelKeys(text)) return reply(400, id);
  if (!body || typeof body !== 'object' || Array.isArray(body) || typeof body.model !== 'string' || !policy.models.includes(body.model)) return reply(403, id);
  // These alter routing, persistence or automatic tool execution outside this plane.
  if (['provider', 'fallbacks', 'background', 'mcp', 'mcp_servers', 'extra_headers'].some(k => k in body)) return reply(400, id);
  const replay = request.headers.get('idempotency-key');
  if (replay !== null && !/^[A-Za-z0-9_-]{16,128}$/.test(replay)) return reply(400, id);
  const headers = headersFor(request, 'inference', id);
  headers.set('x-bf-vk', key);
  headers.set('x-deployment-plane', 'inference');
  // Internal admission metadata is rebuilt, never copied from the caller.
  headers.set('x-deployment-key', keyHash);
  headers.set('x-deployment-ip', await digest(request.headers.get('cf-connecting-ip') ?? 'unknown'));
  headers.set('x-deployment-rpm', String(policy.rpm));
  headers.set('x-deployment-daily', String(policy.daily_requests));
  if (replay) headers.set('x-deployment-replay', await digest(`${keyHash}:${replay}`));
  try {
    return await proxy(new Request(`https://bifrost.internal${inferenceRoutes.get(url.pathname)}`, { method: 'POST', headers, body: bytes, signal: request.signal }));
  } catch { return reply(503, id); }
}

export async function admin(request, env, proxy, keyset) {
  const id = crypto.randomUUID();
  const url = cleanURL(request, env.ADMIN_ORIGIN);
  if (!url || !adminAllowed(request.method, url.pathname)) return reply(404, id);
  try { await accessIdentity(request, env, keyset); } catch { return reply(403, id); }
  if (request.headers.has('upgrade') || request.headers.has('content-encoding') || request.headers.has('x-bf-vk') || request.headers.has('authorization') || request.headers.has('x-api-key')) return reply(403, id);
  if (request.method !== 'GET' && request.headers.get('origin') !== env.ADMIN_ORIGIN) return reply(403, id);
  // Only the OIDC callback may carry an authorization code. Never log its query.
  if (url.search && !['/login', '/api/session/oidc/callback', '/api/config', '/api/providers', '/api/models', '/api/models/details', '/api/models/parameters', '/api/keys', '/api/governance/virtual-keys'].includes(url.pathname)) return reply(400, id);
  if (['/login', '/api/session/oidc/callback'].includes(url.pathname) && (url.search.length > 16384 || new Set(url.searchParams.keys()).size !== [...url.searchParams].length)) return reply(400, id);
  for (const [name, value] of url.searchParams) {
    if (url.pathname === '/api/session/oidc/callback') {
      if (!['code', 'state', 'error', 'error_description', 'error_uri', 'iss'].includes(name) || value.length > (name === 'code' ? 8192 : 1024)) return reply(400, id);
    } else if (url.pathname === '/login') {
      if (name !== 'oidc_error' || !['cancelled', 'invalid', 'unavailable', 'denied'].includes(value)) return reply(400, id);
    } else if (url.pathname === '/api/config') {
      if (name !== 'from_db' || !['true', 'false'].includes(value)) return reply(400, id);
    } else if (!['offset', 'limit', 'search', 'provider', 'model', 'page'].includes(name)) return reply(400, id);
  }
  let bytes;
  try { bytes = await readBody(request, 1024 * 1024); } catch { return reply(413, id); }
  const headers = headersFor(request, 'admin', id);
  headers.set('x-deployment-plane', 'admin');
  try {
    return await proxy(new Request(`https://bifrost.internal${url.pathname}${url.search}`, { method: request.method, headers, body: request.method === 'GET' ? undefined : bytes, signal: request.signal }));
  } catch { return reply(503, id); }
}
