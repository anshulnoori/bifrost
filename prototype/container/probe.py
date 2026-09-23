"""Synthetic data only. The listener is never bound to a container interface."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import signal
import time
import uuid

BOOT = str(uuid.uuid4())


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        if self.path == '/healthz':
            body = json.dumps({'ok': True, 'boot': BOOT}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == '/events':
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Cache-Control', 'no-store')
            self.end_headers()
            try:
                for seq in range(600):
                    body = json.dumps({'boot': BOOT, 'seq': seq})
                    self.wfile.write(f'id: {seq}\ndata: {body}\n\n'.encode())
                    self.wfile.flush()
                    time.sleep(1)
            except (BrokenPipeError, ConnectionResetError):
                pass
        else:
            self.send_error(404)


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, lambda *_: exit(0))
    ThreadingHTTPServer(('127.0.0.1', 8080), Handler).serve_forever()
