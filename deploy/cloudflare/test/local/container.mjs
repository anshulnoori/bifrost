// Synthetic upstream only. No provider or cloud credentials are used here.
import http from 'node:http';
import { randomUUID } from 'node:crypto';

const boot = randomUUID();
let requests = 0, active = 0, cancelled = 0;
const server = http.createServer(async (req, res) => {
  if (req.url === '/health') { res.end('ready'); return; }
  requests++;
  const headers = req.headers;
  if (req.url.startsWith('/api/') || req.url === '/login') {
    if (headers.cookie !== 'token=fixture-session') { res.writeHead(401); res.end(); return; }
  } else if (!headers['x-bf-vk']?.startsWith('sk-bf-fixture-')) {
    res.writeHead(401); res.end(); return;
  }
  let raw = '';
  for await (const chunk of req) raw += chunk;
  const body = raw ? JSON.parse(raw) : {};
  const facts = { boot, requests, active, cancelled, path: req.url,
    // Return only header names and known fixture session state, never credentials.
    headerNames: Object.keys(headers), session: headers.cookie === 'token=fixture-session', raw };
  res.setHeader('content-type', body.stream ? 'text/event-stream' : 'application/json');
  res.setHeader('set-cookie', 'fixture=upstream');
  if (!body.stream) { res.end(JSON.stringify(facts)); return; }
  active++;
  let complete = false;
  res.write(`data: ${JSON.stringify({ boot, sequence: 1 })}\n\n`);
  const interval = setInterval(() => res.write('data: {"heartbeat":true}\n\n'), 100);
  const timer = body.input === 'hold' ? null : setTimeout(() => {
    complete = true;
    res.end('data: [DONE]\n\n');
  }, 350);
  res.on('close', () => { active--; if (!complete) cancelled++; clearInterval(interval); clearTimeout(timer); });
});
// Exercise readiness waits instead of making the mock immediately available.
setTimeout(() => server.listen(8080, '0.0.0.0'), 500);
process.on('SIGTERM', () => {
  server.close(() => process.exit(0));
  setTimeout(() => { server.closeAllConnections(); process.exit(0); }, 1000).unref();
});
