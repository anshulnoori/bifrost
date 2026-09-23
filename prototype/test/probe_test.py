import http.client
import importlib.util
import json
from contextlib import closing
from pathlib import Path
import threading
import unittest

spec = importlib.util.spec_from_file_location('probe', Path(__file__).parents[1] / 'container/probe.py')
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class ProbeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = probe.ThreadingHTTPServer(('127.0.0.1', 0), probe.Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def connection(self):
        return closing(http.client.HTTPConnection(*self.server.server_address, timeout=3))

    def test_health_and_unknown_routes(self):
        with self.connection() as connection:
            connection.request('GET', '/healthz')
            response = connection.getresponse()
            self.assertEqual(response.status, 200)
            self.assertEqual(json.loads(response.read()), {'ok': True, 'boot': probe.BOOT})
        with self.connection() as connection:
            connection.request('GET', '/v1/chat/completions')
            self.assertEqual(connection.getresponse().status, 404)

    def test_sse_order_and_disconnect_cleanup(self):
        with self.connection() as connection:
            connection.request('GET', '/events')
            response = connection.getresponse()
            self.assertEqual(response.getheader('Content-Type'), 'text/event-stream')
            for sequence in range(3):
                self.assertEqual(response.readline(), f'id: {sequence}\n'.encode())
                event = json.loads(response.readline().decode().removeprefix('data: '))
                self.assertEqual(event, {'boot': probe.BOOT, 'seq': sequence})
                self.assertEqual(response.readline(), b'\n')
        with self.connection() as connection:
            connection.request('GET', '/healthz')
            self.assertEqual(connection.getresponse().status, 200)


if __name__ == '__main__':
    unittest.main()
