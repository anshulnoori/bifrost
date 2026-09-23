// Loopback-only test controller, absent from all production routes and bindings.
import { getContainer } from '@cloudflare/containers';
import type { BifrostContainer } from './entry';
export default {
  async fetch(request: Request, env: { BIFROST: DurableObjectNamespace<BifrostContainer> }) {
    const instance = getContainer(env.BIFROST, 'primary');
    const path = new URL(request.url).pathname;
    if (path === '/status') return Response.json(await instance.fixtureStatus());
    if (request.method === 'POST' && path === '/idle') { await instance.fixtureIdle(); return new Response('ok'); }
    if (request.method === 'POST' && path === '/stop') { await instance.fixtureStop(); return new Response('ok'); }
    if (request.method === 'POST' && path === '/reset') { await instance.fixtureReset(); return new Response('ok'); }
    if (request.method === 'POST' && path === '/unready') return Response.json(await instance.fixtureUnready());
    return new Response(null, { status: 404 });
  },
};
