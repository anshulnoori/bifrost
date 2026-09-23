import { Container, getContainer } from '@cloudflare/containers';
import { WorkerEntrypoint } from 'cloudflare:workers';
import { admin, inference, reply } from '../shared/policy.mjs';
import { admit, drainBody } from '../shared/lifecycle.mjs';

interface Env {
  BIFROST: DurableObjectNamespace<BifrostContainer>;
  INFERENCE_ORIGIN: string; ADMIN_ORIGIN: string; EMERGENCY_DISABLE: string;
  INFERENCE_KEYS_JSON: string; ACCESS_ISSUER: string; ACCESS_AUD: string;
  OWNER_EMAIL: string; OWNER_SUB: string;
  NEON_DATABASE_URL: string; BIFROST_ENCRYPTION_KEY: string; BIFROST_ADMIN_PASSWORD: string;
  HEADROOM_ENDPOINT: string; HEADROOM_PROXY_TOKEN: string; HEADROOM_SCOPE_KEY: string;
  HEADROOM_METRICS_TOKEN: string; MODAL_TOKEN_ID: string; MODAL_TOKEN_SECRET: string;
}

export class BifrostContainer extends Container<Env> {
  defaultPort = 8080;
  sleepAfter = '5m';
  enableInternet = true;
  override onError(): void { console.log(JSON.stringify({ event: 'container_error' })); }
  override onStart(): void { console.log(JSON.stringify({ event: 'container_started' })); }
  override onStop(): void { console.log(JSON.stringify({ event: 'container_stopped' })); }

  override async onActivityExpired(): Promise<void> {
    const leases = await this.ctx.storage.list<number>({ prefix: 'lease:' });
    for (const [id, expiry] of leases) {
      if (expiry > Date.now()) { this.renewActivityTimeout(); return; }
      await this.ctx.storage.delete(id);
    }
    // Bounded retention for admission state. No prompts or bearer keys in DO storage.
    for (const [id, value] of await this.ctx.storage.list<number | { until: number }>()) {
      const until = typeof value === 'number' ? value : value.until;
      if (until < Date.now()) await this.ctx.storage.delete(id);
    }
    await this.stop();
  }

  override async fetch(request: Request): Promise<Response> {
    const plane = request.headers.get('x-deployment-plane');
    if (plane !== 'admin' && plane !== 'inference') return reply(403);
    if (plane === 'inference') {
      const code = await admit(this.ctx.storage, request.headers);
      if (code !== 200) return reply(code);
    }
    const envVars: Record<string, string> = {};
    for (const name of ['NEON_DATABASE_URL', 'BIFROST_ENCRYPTION_KEY', 'BIFROST_ADMIN_PASSWORD', 'HEADROOM_ENDPOINT', 'HEADROOM_PROXY_TOKEN', 'HEADROOM_SCOPE_KEY', 'HEADROOM_METRICS_TOKEN', 'MODAL_TOKEN_ID', 'MODAL_TOKEN_SECRET'] as const) {
      if (!this.env[name]) return reply(503);
      envVars[name] = this.env[name];
    }
    const id = crypto.randomUUID();
    const controller = new AbortController();
    const abort = () => controller.abort();
    request.signal.addEventListener('abort', abort, { once: true });
    if (request.signal.aborted) abort();
    const timer = setTimeout(abort, 10 * 60 * 1000);
    const done = async () => {
      clearTimeout(timer);
      request.signal.removeEventListener('abort', abort);
      await this.ctx.storage.delete(`lease:${id}`);
      this.renewActivityTimeout();
    };
    await this.ctx.storage.put(`lease:${id}`, Date.now() + 11 * 60 * 1000);
    try {
      await this.startAndWaitForPorts({ ports: [8080], startOptions: { envVars }, cancellationOptions: {
        abort: controller.signal, instanceGetTimeoutMS: 10000, portReadyTimeoutMS: 45000,
      } });
      const headers = new Headers(request.headers);
      for (const name of [...headers.keys()]) if (name.startsWith('x-deployment-')) headers.delete(name);
      // Direct port fetch after bounded startup: no SDK proxy error strings,
      // exception logging or automatic inference replay.
      const target = request.url.replace('https:', 'http:');
      const response = await this.ctx.container!.getTcpPort(8080).fetch(new Request(target, {
        method: request.method, headers, body: request.body, signal: controller.signal,
      }));
      const out = new Headers(response.headers);
      out.set('cache-control', 'no-store');
      out.set('x-request-id', request.headers.get('x-request-id') ?? id);
      out.delete('server');
      if (plane === 'inference') out.delete('set-cookie');
      return new Response(drainBody(response.body, done, controller.signal), { status: response.status, headers: out });
    } catch { await done(); return reply(503, id); }
  }
}

export class AdminOrigin extends WorkerEntrypoint<Env> {
  override fetch(request: Request): Promise<Response> {
    return admin(request, this.env, (r: Request) => getContainer(this.env.BIFROST, 'primary').fetch(r), undefined);
  }
}

export default {
  fetch(request: Request, env: Env): Promise<Response> {
    return inference(request, env, (r: Request) => getContainer(env.BIFROST, 'primary').fetch(r));
  },
};
