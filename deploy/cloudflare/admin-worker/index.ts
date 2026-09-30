import { accessIdentity, adminAllowed, cleanURL, reply } from '../shared/policy.mjs';

interface Env {
  ADMIN_ORIGIN: string; ACCESS_ISSUER: string; ACCESS_AUD: string;
  OWNER_EMAIL: string; OWNER_SUB: string; ORIGIN: Fetcher;
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = cleanURL(request, env.ADMIN_ORIGIN);
    if (!url || !adminAllowed(request.method, url.pathname)) return reply(404);
    try { await accessIdentity(request, env, undefined); } catch { return reply(403); }
    // Private service binding; the origin entrypoint repeats JWT verification.
    return env.ORIGIN.fetch(request);
  },
};
