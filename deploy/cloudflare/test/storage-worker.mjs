import { DurableObject } from 'cloudflare:workers';
import { admit } from '../shared/lifecycle.mjs';
export class Ledger extends DurableObject {
  async fetch(request) { return new Response(null, { status: await admit(this.ctx.storage, request.headers) }); }
}
export default { fetch(request, env) { return env.LEDGER.get(env.LEDGER.idFromName('primary')).fetch(request); } };
