import asyncio
import io
import json
import os
import socket
import struct
import sys
import tempfile
import threading
import unittest
import uuid
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch, AsyncMock
import httpx
from service import create_service, isolated_compress, compress_until_disconnect, MAX_BODY
from gpu import GPUCompressor
from gpu_worker import serve, load_compressor

BODY = {"model": "gpt-4.1", "messages": [{"role": "tool", "tool_call_id": "slot-0", "content": "original content"}],
        "config": {"protect_recent": 0, "compress_user_messages": False},
        "gateway": {"can_redrive": False, "can_relay_response": False, "session_affinity": False, "plugin_version": "bifrost-headroom/1"}}
HEADERS = {"x-headroom-proxy-token": "fixture" * 8, "x-headroom-project": "a" * 64}


class ServiceTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.env = patch.dict(os.environ, HEADROOM_PROXY_TOKEN=HEADERS["x-headroom-proxy-token"])
        self.env.start()
        self.calls = 0

        async def compressor(raw):
            self.calls += 1
            body = json.loads(raw)
            body["messages"][0]["content"] = "short"
            return json.dumps({"messages": body["messages"], "tokens_before": 99, "tokens_after": 20}).encode()

        self.client = httpx.AsyncClient(transport=httpx.ASGITransport(app=create_service(compressor)), base_url="http://service")

    async def asyncTearDown(self):
        await self.client.aclose()
        self.env.stop()

    async def test_auth_route_and_body_limits(self):
        self.assertEqual((await self.client.post("/v1/compress", json=BODY)).status_code, 401)
        for path in ["/docs", "/v1/retrieve", "/metrics", "/v1/messages", "/v1/toin/patterns"]:
            self.assertEqual((await self.client.get(path, headers=HEADERS)).status_code, 404)
        self.assertEqual((await self.client.post("/v1/compress", headers=HEADERS, json={**BODY, "config": {"mode": "ccr"}})).status_code, 400)
        self.assertEqual((await self.client.post("/v1/compress", headers={**HEADERS, "content-type": "application/json"}, content=b"x" * (MAX_BODY + 1))).status_code, 413)
        self.assertEqual(self.calls, 0)

    async def test_compression_contract(self):
        res = await self.client.post("/v1/compress", headers=HEADERS, json=BODY)
        self.assertEqual(res.status_code, 200)
        self.assertEqual(res.json()["messages"][0], {"role": "tool", "tool_call_id": "slot-0", "content": "short"})
        self.assertEqual(res.json()["tokens_before"], 99)
        self.assertEqual(res.json()["ccr_hashes"], [])

    async def test_embedding_auth_shape_and_disabled_fallback(self):
        embed = AsyncMock(return_value=[[1.0] + [0.0] * 383])
        body = {"model": "headroom-minilm-v1", "input": "private query"}
        self.assertEqual((await self.client.post("/v1/embeddings", headers=HEADERS, json=body)).status_code, 404)
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=create_service(embed=embed)), base_url="http://service") as client:
            self.assertEqual((await client.post("/v1/embeddings", json=body)).status_code, 401)
            for invalid in [{**body, "dimensions": 3}, {**body, "input": [1, 2]}, {**body, "encoding_format": "base64"}, {**body, "user": "other"}]:
                self.assertEqual((await client.post("/v1/embeddings", headers=HEADERS, json=invalid)).status_code, 400)
            embed.assert_not_awaited()
            response = await client.post("/v1/embeddings", headers=HEADERS, json=body)
            self.assertEqual(response.status_code, 200)
            self.assertEqual(response.json()["data"], [{"object": "embedding", "index": 0, "embedding": [1.0] + [0.0] * 383}])
            embed.assert_awaited_once_with(["private query"])
            embed.side_effect = ValueError("unavailable")
            self.assertEqual((await client.post("/v1/embeddings", headers=HEADERS, json=body)).status_code, 502)

    async def test_timeout_and_corruption_rejected(self):
        async def timeout(_): raise asyncio.TimeoutError()
        async def corruption(_): return b'{"messages": [], "tokens_before": 9, "tokens_after": 1}'
        for compressor, status in [(timeout, 504), (corruption, 502)]:
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=create_service(compressor)), base_url="http://service") as client:
                self.assertEqual((await client.post("/v1/compress", headers=HEADERS, json=BODY)).status_code, status)

    async def test_chunked_limit_before_compressor(self):
        async def chunks():
            yield b"x" * MAX_BODY
            yield b"x"
        response = await self.client.post("/v1/compress", headers={**HEADERS, "content-type": "application/json"}, content=chunks())
        self.assertEqual(response.status_code, 413)
        self.assertEqual(self.calls, 0)

    async def test_concurrency_cap_and_slot_release(self):
        entered, release = asyncio.Event(), asyncio.Event()

        async def blocked(raw):
            entered.set()
            await release.wait()
            return json.dumps({"messages": json.loads(raw)["messages"], "tokens_before": 9, "tokens_after": 9}).encode()

        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=create_service(blocked)), base_url="http://service") as client:
            first = asyncio.create_task(client.post("/v1/compress", headers=HEADERS, json=BODY))
            try:
                await asyncio.wait_for(entered.wait(), 2)
                second = await client.post("/v1/compress", headers=HEADERS, json=BODY)
                self.assertEqual(second.status_code, 429)
                self.assertEqual(second.headers["retry-after"], "1")
            finally:
                release.set()
                self.assertEqual((await first).status_code, 200)
            self.assertEqual((await client.post("/v1/compress", headers=HEADERS, json=BODY)).status_code, 200)

    @unittest.skipUnless(os.environ.get("HEADROOM_REAL_TEST") == "1", "opt-in official package integration")
    async def test_official_headroom_fidelity(self):
        text = json.dumps([{"id": i, "status": "healthy", "target_fact": "KEEP-7391", "region": "east"} for i in range(150)])
        body = {**BODY, "messages": [{**BODY["messages"][0], "content": text}]}
        result = json.loads(await isolated_compress(json.dumps(body).encode()))
        self.assertIn("KEEP-7391", result["messages"][0]["content"])
        self.assertLess(result["tokens_after"], result["tokens_before"])
        self.assertFalse(result.get("ccr_hashes"))


class GPUWorkerTests(unittest.IsolatedAsyncioTestCase):
    async def test_cancel_waits_for_database_thread(self):
        from features import state_call
        entered, release, finished = threading.Event(), threading.Event(), threading.Event()
        def transaction():
            entered.set()
            release.wait(2)
            finished.set()
        task = asyncio.create_task(state_call(transaction))
        try:
            while not entered.is_set():
                await asyncio.sleep(0.001)
            task.cancel()
            await asyncio.sleep(0.01)
            self.assertFalse(task.done(), "a cancelled request must still own the DB work")
        finally:
            release.set()
            with self.assertRaises(asyncio.CancelledError):
                await task
        self.assertTrue(finished.is_set())

    async def test_http_disconnect_cancels_compression(self):
        import uvicorn
        entered, cleaned = asyncio.Event(), asyncio.Event()

        async def blocked(_):
            try:
                entered.set()
                await asyncio.Event().wait()
            finally:
                cleaned.set()

        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        sock.setblocking(False)
        server = uvicorn.Server(uvicorn.Config(create_service(blocked), log_level="critical", access_log=False))
        with patch.dict(os.environ, HEADROOM_PROXY_TOKEN=HEADERS["x-headroom-proxy-token"]):
            serving = asyncio.create_task(server.serve(sockets=[sock]))
            try:
                for _ in range(100):
                    if server.started:
                        break
                    await asyncio.sleep(0.01)
                self.assertTrue(server.started)
                _, writer = await asyncio.open_connection("127.0.0.1", sock.getsockname()[1])
                raw = json.dumps(BODY).encode()
                headers = {**HEADERS, "host": "localhost", "content-type": "application/json", "content-length": str(len(raw))}
                writer.write(("POST /v1/compress HTTP/1.1\r\n" + "".join(f"{k}: {v}\r\n" for k, v in headers.items()) + "\r\n").encode() + raw)
                await writer.drain()
                await asyncio.wait_for(entered.wait(), 2)
                writer.close()
                await writer.wait_closed()
                await asyncio.wait_for(cleaned.wait(), 2)
            finally:
                server.should_exit = True
                await asyncio.wait_for(serving, 5)
                sock.close()

    async def test_disconnect_waits_for_compressor_cleanup(self):
        entered, cleaned = asyncio.Event(), asyncio.Event()

        async def blocked(_):
            try:
                entered.set()
                await asyncio.Event().wait()
            finally:
                await asyncio.sleep(0)
                cleaned.set()

        async def disconnected():
            await entered.wait()
            return {"type": "http.disconnect"}

        result = await compress_until_disconnect(SimpleNamespace(receive=disconnected), b"{}", blocked)
        self.assertIsNone(result)
        self.assertTrue(cleaned.is_set())

    def test_framing_reuses_weights_without_response_state(self):
        class Compressor:
            def compress(self, text, *, allow_download):
                self.last_download = allow_download
                return SimpleNamespace(compressed=text[:5], original_tokens=20, compressed_tokens=3)

        compressor = Compressor()
        bodies = [json.dumps({**BODY, "messages": [{**BODY["messages"][0], "content": text}]}).encode()
                  for text in ["ALPHA confidential", "BRAVO confidential"]]
        source = io.BytesIO(b"".join(struct.pack("!I", len(body)) + body for body in bodies))
        output = io.BytesIO()
        serve(source, output, compressor)
        output.seek(0)
        self.assertEqual(output.read(5), b"CUDA\n")
        for expected in ["ALPHA", "BRAVO"]:
            size = struct.unpack("!I", output.read(4))[0]
            result = json.loads(output.read(size))
            self.assertEqual(result["messages"][0]["content"], expected)
            self.assertEqual((result["tokens_before"], result["tokens_after"]), (20, 3))
            self.assertEqual(result["ccr_hashes"], [])
        self.assertFalse(compressor.last_download)
        for frame in [b"x", struct.pack("!I", MAX_BODY + 1), struct.pack("!I", 3) + b"x"]:
            with self.assertRaises(ValueError):
                serve(io.BytesIO(frame), io.BytesIO(), compressor)

    def test_loader_requires_cuda_and_explicit_marker_free_pytorch(self):
        selected = []
        class Compressor:
            def __init__(self, config): selected.append(config)
            def preload(self, *, allow_download):
                self.assert_offline = not allow_download
                return "pytorch"
        torch = SimpleNamespace(cuda=SimpleNamespace(is_available=lambda: True))
        module = SimpleNamespace(KompressCompressor=Compressor, KompressConfig=lambda **kw: kw)
        with patch.dict(sys.modules, {"torch": torch, "headroom.transforms.kompress_compressor": module}):
            self.assertTrue(load_compressor().assert_offline)
            self.assertEqual(selected, [{"device": "cuda", "enable_ccr": False}])
            torch.cuda.is_available = lambda: False
            with self.assertRaises(RuntimeError):
                load_compressor()

    async def test_cancel_kills_worker_and_removes_private_scratch(self):
        worker = GPUCompressor()
        worker.scratch = tempfile.TemporaryDirectory()
        scratch = Path(worker.scratch.name)
        # Real disposable subprocess, no CUDA needed to test process lifetime.
        worker.process = await asyncio.create_subprocess_exec(sys.executable, "-c", "import time; time.sleep(60)",
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL)
        process = worker.process
        request = asyncio.create_task(worker(b"{}"))
        await asyncio.sleep(0.05)
        request.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await request
        self.assertIsNotNone(process.returncode)
        self.assertIsNone(worker.process)
        self.assertFalse(scratch.exists())

    async def test_worker_environment_excludes_service_credentials(self):
        async def spawn(*args, **kwargs):
            env = kwargs["env"]
            self.assertNotIn("MODAL_TOKEN_SECRET", env)
            self.assertNotIn("HEADROOM_PROXY_TOKEN", env)
            self.assertEqual(env["HEADROOM_KOMPRESS_BACKEND"], "pytorch")
            self.assertEqual(env["HF_HUB_OFFLINE"], "1")
            return SimpleNamespace(stdout=SimpleNamespace(readexactly=AsyncMock(return_value=b"CUDA\n")),
                                   returncode=0, wait=AsyncMock())
        worker = GPUCompressor()
        with patch.dict(os.environ, MODAL_TOKEN_SECRET="synthetic", HEADROOM_PROXY_TOKEN="synthetic"), \
                patch("gpu.asyncio.create_subprocess_exec", side_effect=spawn):
            await worker.warmup()
            await worker.close()


@unittest.skipUnless(os.environ.get("HEADROOM_TEST_POSTGRES") == "1", "disposable loopback PostgreSQL only")
class StateStoreTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import psycopg
        from psycopg import sql
        from state import StateStore
        cls.database = "headroom_test_" + uuid.uuid4().hex
        cls.admin = "host=127.0.0.1 user=postgres password=fixture sslmode=disable"
        with psycopg.connect(cls.admin, autocommit=True) as connection:
            cls.created_role = connection.execute("SELECT 1 FROM pg_roles WHERE rolname = 'headroom_runtime'").fetchone() is None
            if cls.created_role:
                connection.execute("CREATE ROLE headroom_runtime")
            connection.execute(sql.SQL("CREATE DATABASE {} ").format(sql.Identifier(cls.database)))
        cls.dsn = cls.admin + " dbname=" + cls.database
        with psycopg.connect(cls.dsn, autocommit=True) as connection:
            connection.execute(Path(__file__).resolve().parents[2].joinpath("neon/headroom-state.sql").read_text())

        def restricted(dsn, **kwargs):
            connection = psycopg.connect(dsn, **kwargs)
            connection.execute("SET ROLE headroom_runtime")
            return connection

        cls.store = StateStore(cls.dsn, b"x" * 32, connect=restricted)

    @classmethod
    def tearDownClass(cls):
        import psycopg
        from psycopg import sql
        with psycopg.connect(cls.admin, autocommit=True) as connection:
            connection.execute(sql.SQL("DROP DATABASE {} ").format(sql.Identifier(cls.database)))
            if cls.created_role:
                connection.execute("DROP ROLE headroom_runtime")

    def setUp(self):
        self.scope = uuid.uuid4().hex * 2

    def test_official_memory_replay_scope_update_delete(self):
        from features import StateFeatures
        from state import StateError
        features = StateFeatures(self.store, AsyncMock(return_value=[[1.0] + [0.0] * 383]))
        def call(name, args, operation=None, scope=None):
            return asyncio.run(features.tool(scope or self.scope, name, args, operation or uuid.uuid4().hex * 2))
        identifier = "c" * 64
        args = {"content": "prefer private blue reports", "importance": 0.8}
        saved = call("memory_save", args, identifier)
        self.assertEqual(saved, {"success": True, "memory_id": identifier})
        self.assertEqual(call("memory_save", args, identifier), saved)
        with self.assertRaises(StateError):
            call("memory_save", {**args, "content": "changed"}, identifier)
        self.assertEqual(call("memory_search", {"query": "reports"}, scope="b" * 64)["count"], 0)
        self.assertEqual(call("memory_search", {"query": "reports"})["count"], 1)
        updated = call("memory_update", {"memory_id": identifier, "new_content": "prefer green reports", "reason": "changed preference"})
        self.assertNotEqual(updated["memory_id"], identifier)
        self.assertEqual(call("memory_search", {"query": "reports"})["count"], 1)
        call("memory_delete", {"memory_id": updated["memory_id"], "reason": "forget"})
        self.assertEqual(call("memory_search", {"query": "reports"})["count"], 0)
        self.assertEqual(call("memory_save", args, identifier), saved)
        with self.store.transaction(self.scope) as state:
            self.assertEqual(state.list("memory"), [])
            journals = json.dumps(state.list("operation"))
            self.assertNotIn("reports", journals)

    def test_ccr_handles_require_original_scope(self):
        from features import StateFeatures
        from state import StateError
        features = StateFeatures(self.store, None)
        original = "private detail " * 100
        texts, handles = features.retain(self.scope, [original, "tiny"], ["summary", "tiny"])
        self.assertIn(handles[0], texts[0])
        self.assertEqual(handles[1], "")
        result = asyncio.run(features.tool(self.scope, "headroom_retrieve", {"hash": handles[0]}, "d" * 64))
        self.assertEqual(result["content"], original)
        with self.assertRaises(StateError):
            asyncio.run(features.tool("b" * 64, "headroom_retrieve", {"hash": handles[0]}, "d" * 64))

    def test_authenticated_facade_ccr_negotiation_and_retrieval(self):
        from features import StateFeatures
        from service import CCR_GATEWAY, GATEWAY
        original = "private omitted detail " * 100
        async def compressor(raw):
            body = json.loads(raw)
            self.assertEqual(body["gateway"], GATEWAY)
            self.assertNotIn("ccr", body["config"])
            return json.dumps({"messages": [{**body["messages"][0], "content": "summary"}], "tokens_before": 900, "tokens_after": 10}).encode()
        async def exercise():
            features = StateFeatures(self.store, None)
            app = create_service(compressor, features=features)
            headers = {**HEADERS, "x-headroom-project": self.scope}
            body = {**BODY, "messages": [{**BODY["messages"][0], "content": original}],
                    "gateway": CCR_GATEWAY, "config": {**BODY["config"], "ccr": True}}
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://service") as client:
                result = await client.post("/v1/compress", headers=headers, json=body)
                self.assertEqual(result.status_code, 200)
                handle = result.json()["ccr_hashes"][0]
                self.assertIn(handle, result.json()["messages"][0]["content"])
                tool = {"name": "headroom_retrieve", "arguments": {"hash": handle}, "operation_id": "d" * 64}
                self.assertEqual((await client.post("/v1/tools", json=tool)).status_code, 401)
                self.assertEqual((await client.post("/v1/tools", headers={**headers, "x-headroom-project": "b" * 64}, json=tool)).status_code, 502)
                retrieved = await client.post("/v1/tools", headers=headers, json=tool)
                self.assertEqual(retrieved.json()["content"], original)
        with patch.dict(os.environ, HEADROOM_PROXY_TOKEN=HEADERS["x-headroom-proxy-token"]):
            asyncio.run(exercise())

    def test_restart_isolation_ciphertext_and_tamper(self):
        import psycopg
        from state import StateStore, StateError
        secret = {"content": "private-original-7391"}
        with self.store.transaction(self.scope) as state:
            state.put("ccr", "aa", secret, 60)
        restarted = StateStore(self.dsn, b"x" * 32, connect=self.store.connect)
        with restarted.transaction(self.scope) as state:
            self.assertEqual(state.get("ccr", "aa"), secret)
        other = uuid.uuid4().hex * 2
        with restarted.transaction(other) as state:
            self.assertIsNone(state.get("ccr", "aa"))
        with psycopg.connect(self.dsn) as connection:
            raw = connection.execute("SELECT ciphertext FROM headroom.entries WHERE scope = %s", (self.scope,)).fetchone()[0]
            self.assertNotIn(b"private-original", bytes(raw))
            connection.execute("UPDATE headroom.entries SET scope = %s WHERE scope = %s", (other, self.scope))
        with self.assertRaisesRegex(StateError, "^state operation failed$"):
            with restarted.transaction(other) as state:
                state.get("ccr", "aa")

    def test_expiry_quota_and_rollback(self):
        from state import StateError
        with patch("state.time.time", return_value=1):
            with self.store.transaction(self.scope) as state:
                state.put("ccr", "aa", "expired", 1)
        with self.store.transaction(self.scope) as state:
            self.assertIsNone(state.get("ccr", "aa"))
        with patch("state.MAX_SCOPE_ENTRIES", 1):
            with self.assertRaises(StateError):
                with self.store.transaction(self.scope) as state:
                    state.put("memory", "bb", "one", 60)
                    state.put("memory", "cc", "two", 60)
        with self.store.transaction(self.scope) as state:
            self.assertEqual(state.list("memory"), [])

    def test_concurrent_updates_serialize(self):
        def increment(_):
            with self.store.transaction(self.scope) as state:
                count = state.get("learning", "1") or 0
                state.put("learning", "1", count + 1, 60)
        with ThreadPoolExecutor(max_workers=4) as pool:
            list(pool.map(increment, range(12)))
        with self.store.transaction(self.scope) as state:
            self.assertEqual(state.get("learning", "1"), 12)
            self.assertTrue(state.delete("learning", "1"))
            self.assertFalse(state.delete("learning", "1"))


if __name__ == "__main__":
    unittest.main()
