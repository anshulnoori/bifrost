const encoder = new TextEncoder();
export const canonical = (origin, path, time, nonce) =>
  `tailnet-lifecycle-v1\n${origin}\nPOST\n${path}\n${time}\n${nonce}`;

export async function signature(secret, message) {
  const key = await crypto.subtle.importKey(
    'raw', encoder.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'],
  );
  return Array.from(new Uint8Array(await crypto.subtle.sign('HMAC', key, encoder.encode(message))),
    n => n.toString(16).padStart(2, '0')).join('');
}

export async function authenticate(request, secret, now = Date.now()) {
  const url = new URL(request.url);
  if (url.protocol !== 'https:' || url.search || !['/wake', '/status'].includes(url.pathname) ||
      request.method !== 'POST' ||
      request.headers.has('transfer-encoding') ||
      !['0', null].includes(request.headers.get('content-length'))) return null;
  if (!/^[a-f0-9]{64}$/.test(secret ?? '')) return null;
  const time = request.headers.get('x-wake-time') ?? '';
  const nonce = request.headers.get('x-wake-nonce') ?? '';
  const sig = request.headers.get('x-wake-signature') ?? '';
  if (!/^\d{10}$/.test(time) || !/^[a-f0-9]{32}$/.test(nonce) || !/^[a-f0-9]{64}$/.test(sig) ||
      Math.abs(Math.floor(now / 1000) - Number(time)) > 30) return null;
  const key = await crypto.subtle.importKey(
    'raw', encoder.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['verify'],
  );
  const valid = await crypto.subtle.verify('HMAC', key,
    Uint8Array.from(sig.match(/../g), hex => parseInt(hex, 16)),
    encoder.encode(canonical(url.origin, url.pathname, time, nonce)));
  if (!valid) return null;
  // workerd represents even an empty POST as a stream. Reject bytes without parsing them.
  if (request.body !== null) {
    if (request.headers.get('content-length') !== '0') return null;
    const reader = request.body.getReader();
    const first = await reader.read();
    if (!first.done) {
      await reader.cancel();
      return null;
    }
  }
  return { action: url.pathname.slice(1), nonce, time: Number(time) };
}

// Run inside a Durable Object storage transaction, never an isolate-local cache.
export async function consume(storage, ticket, now = Date.now()) {
  if (Math.abs(Math.floor(now / 1000) - ticket.time) > 30) return 401;
  const nonces = await storage.list({ prefix: 'nonce:' });
  for (const [key, expiry] of nonces) if (expiry < now) await storage.delete(key);
  const key = `nonce:${ticket.nonce}`;
  if (await storage.get(key) !== undefined) return 409;
  const bucket = Math.floor(now / 60_000);
  const previous = await storage.get('rate');
  const rate = previous?.bucket === bucket ? previous : { bucket, all: 0, wake: 0 };
  if (rate.all >= 60 || (ticket.action === 'wake' && rate.wake >= 6)) return 429;
  rate.all++;
  if (ticket.action === 'wake') rate.wake++;
  await storage.put('rate', rate);
  // Retain until this signed timestamp cannot pass verification, including future skew.
  await storage.put(key, (ticket.time + 31) * 1000);
  return 200;
}
