import { authenticate } from './auth.mjs';

export const reply = (status, body = { status: 'unavailable' }) => Response.json(body, {
  status, headers: { 'cache-control': 'no-store', 'x-content-type-options': 'nosniff' },
});

export async function handle(request, env) {
  try {
    const ticket = await authenticate(request, env.WAKE_HMAC_KEY);
    if (!ticket) return reply(404);
    // Fixed singleton. No request URL, headers or body enters the container.
    const result = await env.PROBE.getByName('private-proof').control(ticket);
    return reply(result.code, { status: result.status });
  } catch {
    return reply(503);
  }
}
