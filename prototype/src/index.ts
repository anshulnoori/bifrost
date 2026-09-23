import { Container } from '@cloudflare/containers';
import { consume } from './auth.mjs';
import { handle, reply } from './handler.mjs';

interface Env {
  PROBE: DurableObjectNamespace<PrivateProbe>;
  TS_AUTHKEY: string;
  WAKE_HMAC_KEY: string;
}

export class PrivateProbe extends Container<Env> {
  // Intentionally unmodified: Stage 1 must demonstrate private traffic timing out.
  sleepAfter = '120s';
  enableInternet = true;

  override async fetch(): Promise<Response> {
    return reply(404); // Never inherit the SDK's default container proxy.
  }

  async control(ticket: { action: string; nonce: string; time: number }) {
    const code = await this.ctx.storage.transaction(tx => consume(tx, ticket));
    if (code !== 200) return { code, status: 'rejected' };
    if (ticket.action === 'wake') {
      if (!this.env.TS_AUTHKEY) return { code: 503, status: 'unconfigured' };
      await this.start({ envVars: { TS_AUTHKEY: this.env.TS_AUTHKEY } });
      this.renewActivityTimeout();
    }
    // Do not probe an application port, report readiness, or renew on status reads.
    return { code: 200, status: this.ctx.container?.running ? 'running' : 'stopped' };
  }

  override onError(): void {
    // SDK exceptions can include runtime configuration. Never log them verbatim.
    console.log('container_error');
  }
  override onStart(): void { console.log('container_started'); }
  override onStop(): void { console.log('container_stopped'); }
}

export default { fetch: handle };
