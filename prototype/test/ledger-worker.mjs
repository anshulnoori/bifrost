import { DurableObject } from 'cloudflare:workers';
import { consume } from '../src/auth.mjs';
import { handle } from '../src/handler.mjs';

// Real SQLite Durable Object storage, mocked container lifecycle only.
export class Ledger extends DurableObject {
  async control(ticket) {
    const code = await this.ctx.storage.transaction(tx => consume(tx, ticket));
    return { code, status: code === 200 ? 'stopped' : 'rejected' };
  }
}
export default { fetch: handle };
