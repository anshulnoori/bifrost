// Restricted ingress-only transport fixture for kernels without xt_socket/TPROXY.
// This is NOT a Cloudflare egress emulator. No outbound destinations are supported.
import http from 'node:http';
import net from 'node:net';
import { rootCertificates } from 'node:tls';

const args = process.argv.slice(2);
let address;
for (let i = 0; i < args.length; i++) {
  if (/^-{1,2}http-ingress-address$/.test(args[i])) address = args[++i];
  else if (/^-{1,2}http-ingress-address=/.test(args[i])) address = args[i].split('=')[1];
}
if (!address) throw new Error('local ingress address required');
const port = Number(address.split(':').at(-1));
const server = http.createServer((req, res) => {
  req.resume();
  // Miniflare control handshake only. No TLS interception or egress is emulated.
  // Its handshake requires parseable CA bytes even though this fixture does no TLS.
  if (req.method === 'GET' && req.url === '/ca') { res.end(rootCertificates[0]); return; }
  res.writeHead(req.method === 'PUT' && req.url === '/egress' ? 204 : 404);
  res.end();
});
server.on('connect', (req, socket, head) => {
  const target = req.headers['x-dst-addr'];
  if (!['127.0.0.1:8080', '127.0.0.1:6553'].includes(target)) { socket.end('HTTP/1.1 403 Forbidden\r\n\r\n'); return; }
  const upstream = net.connect(Number(target.split(':')[1]), '127.0.0.1');
  let connected = false;
  upstream.on('connect', () => {
    connected = true;
    socket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
    if (head.length) upstream.write(head);
    socket.pipe(upstream).pipe(socket);
  });
  // Match the stock CONNECT protocol: closed ports return 400, not a reset that
  // the SDK interprets as loss of its container runtime connection.
  upstream.on('error', () => connected ? socket.destroy() : socket.end('HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n'));
  socket.on('error', () => upstream.destroy());
  socket.on('close', () => upstream.destroy());
  upstream.on('close', () => { if (connected) socket.destroy(); });
});
server.listen(port, '0.0.0.0');
process.on('SIGTERM', () => { server.close(); process.exit(0); });
