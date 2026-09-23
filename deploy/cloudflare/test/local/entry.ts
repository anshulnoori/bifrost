// Test-only subclass. Never referenced by the deployment configurations.
import worker, { BifrostContainer as ProductionContainer, AdminOrigin } from '../../inference-worker/index';
export { AdminOrigin };
export default worker;
export class BifrostContainer extends ProductionContainer {
  async fixtureStatus() {
    return { state: await this.getState(), leases: (await this.ctx.storage.list({ prefix: 'lease:' })).size };
  }
  async fixtureIdle() { await this.onActivityExpired(); }
  async fixtureStop() { await this.stop(); }
  async fixtureUnready() {
    const start = this.startAndWaitForPorts.bind(this);
    this.startAndWaitForPorts = () => start({ ports: [6553], cancellationOptions: { instanceGetTimeoutMS: 5000, portReadyTimeoutMS: 500 } });
    this.startupTimeoutMS = 1000;
    try {
      const response = await super.fetch(new Request('http://bifrost.internal/v1/responses', { method: 'POST', headers: { 'x-deployment-plane': 'admin' }, body: '{}' }));
      return { status: response.status };
    } finally { this.startAndWaitForPorts = start; this.startupTimeoutMS = 55000; }
  }
  async fixtureReset() {
    if ((await this.getState()).status !== 'stopped') throw new Error('stop fixture before reset');
    for (const name of (await this.ctx.storage.list()).keys()) {
      if (/^(key|ip|day|replay|lease):/.test(name)) await this.ctx.storage.delete(name);
    }
  }
}
