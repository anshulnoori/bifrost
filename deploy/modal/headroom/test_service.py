import asyncio
import base64
import io
import json
import os
import socket
import struct
import sys
import tempfile
import threading
import time
import unittest
import uuid
import zlib
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch, AsyncMock, MagicMock
import httpx
from encoder import EmbeddingInputError
from service import create_service, isolated_compress, compress_until_disconnect, MAX_BODY
from gpu import GPUCompressor
from gpu_worker import serve, load_compressor
from modal_private import PrivateTransport

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

    async def test_expired_and_invalid_deadlines_do_not_compute(self):
        for deadline, status in [("1", 408), ("invalid", 400)]:
            res = await self.client.post("/v1/compress", headers={**HEADERS, "x-headroom-deadline-ms": deadline}, json=BODY)
            self.assertEqual(res.status_code, status)
        self.assertEqual(self.calls, 0)

    async def test_deadline_expired_during_body_read_does_not_start_work(self):
        work = AsyncMock(return_value=b"{}")
        request = SimpleNamespace(headers={"x-headroom-deadline-ms": "1"}, receive=AsyncMock(return_value={"type": "http.disconnect"}))
        with self.assertRaises(asyncio.TimeoutError):
            await compress_until_disconnect(request, b"{}", work)
        work.assert_not_awaited()

    async def test_deadline_cancels_work_and_cost_logs_exclude_payloads(self):
        cleaned = asyncio.Event()
        async def blocked(_):
            try:
                await asyncio.Event().wait()
            finally:
                cleaned.set()
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=create_service(blocked, cost_per_second=0.00019908)), base_url="http://service") as client:
            output = io.StringIO()
            with patch("sys.stdout", output):
                res = await client.post("/v1/compress", headers={**HEADERS, "x-headroom-deadline-ms": str(int(time.time() * 1000) + 100)}, json=BODY)
            self.assertEqual(res.status_code, 504)
            self.assertTrue(cleaned.is_set())
            record = json.loads(output.getvalue())
            self.assertEqual(record["status"], 504)
            self.assertGreater(record["estimated_active_usd"], 0)
            self.assertTrue(record["excludes_startup_idle_builds"])
            for secret in [HEADERS["x-headroom-proxy-token"], HEADERS["x-headroom-project"], "original content"]:
                self.assertNotIn(secret, output.getvalue())

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


class PrivateTransportTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.env = patch.dict(os.environ, HEADROOM_PROXY_TOKEN=HEADERS["x-headroom-proxy-token"])
        self.env.start()
        self.calls = 0
        self.cleaned = asyncio.Event()

        async def compressor(raw):
            self.calls += 1
            body = json.loads(raw)
            body["messages"][0]["content"] = "short"
            return json.dumps({"messages": body["messages"], "tokens_before": 99, "tokens_after": 20}).encode()

        self.transport = PrivateTransport(create_service(compressor))

    async def asyncTearDown(self):
        self.env.stop()

    async def test_private_success_returns_string_envelope(self):
        result = json.loads(await self.transport.request(
            "/v1/compress", json.dumps(BODY), HEADERS["x-headroom-project"],
            int(time.time() * 1000) + 5_000))
        self.assertEqual(result["status"], 200)
        self.assertEqual(json.loads(result["body"])["messages"][0]["content"], "short")
        self.assertEqual(self.calls, 1)

    async def test_packed_replies_are_opt_in_and_lossless(self):
        async def compressor(raw):
            body = json.loads(raw)
            return json.dumps({"messages": body["messages"], "tokens_before": 99, "tokens_after": 99}).encode()
        transport = PrivateTransport(create_service(compressor))
        body = {**BODY, "messages": [{**BODY["messages"][0], "content": "é sample " * 1000}]}
        raw = json.dumps(body)
        packed = "zlib:" + base64.b64encode(zlib.compress(raw.encode())).decode()
        args = (HEADERS["x-headroom-project"], int(time.time() * 1000) + 5_000)
        plain_reply = await transport.request("/v1/compress", raw, *args)
        packed_reply = await transport.request("/v1/compress", packed, *args)
        self.assertTrue(packed_reply.startswith("zlib:"))
        self.assertEqual(zlib.decompress(base64.b64decode(packed_reply[5:])).decode(), plain_reply)

    async def test_short_embedding_request_can_pack_large_vector_reply(self):
        vectors = [[(i - 193) / 1000000000009 for i in range(384)] for _ in range(2)]
        transport = PrivateTransport(create_service(embed=AsyncMock(return_value=vectors)))
        raw = json.dumps({"model": "headroom-minilm-v1", "input": ["hello", "world"]})
        self.assertLess(len(raw), 4096)
        packed = "zlib:" + base64.b64encode(zlib.compress(raw.encode())).decode()
        args = ("", int(time.time() * 1000) + 5_000)
        plain_reply = await transport.request("/v1/embeddings", raw, *args)
        packed_reply = await transport.request("/v1/embeddings", packed, *args)
        self.assertGreater(len(plain_reply), 8192)
        self.assertTrue(packed_reply.startswith("zlib:"))
        self.assertLess(len(packed_reply), 7000)
        self.assertEqual(zlib.decompress(base64.b64decode(packed_reply[5:])).decode(), plain_reply)

    async def test_private_rejects_expired_unsupported_and_large_before_work(self):
        future = int(time.time() * 1000) + 5_000
        cases = [
            (([], "{}", HEADERS["x-headroom-project"], future), 400),
            (("/health", "{}", HEADERS["x-headroom-project"], future), 404),
            (("/v1/compress", "x" * (MAX_BODY + 1), HEADERS["x-headroom-project"], future), 413),
            (("/v1/compress", json.dumps(BODY), HEADERS["x-headroom-project"], 1), 408),
        ]
        for arguments, status in cases:
            self.assertEqual(json.loads(await self.transport.request(*arguments))["status"], status)
        self.assertEqual(self.calls, 0)

    async def test_private_lossless_transport_bounds(self):
        raw = json.dumps(BODY).encode()
        packed = zlib.compress(raw)
        future = int(time.time() * 1000) + 5_000
        for payload, status in [(packed, 200), (packed[:-1], 400),
                                (packed + b"trailing", 400),
                                (zlib.compress(b"x" * (MAX_BODY + 1)), 413)]:
            result = json.loads(await self.transport.request(
                "/v1/compress", "zlib:" + base64.b64encode(payload).decode(),
                HEADERS["x-headroom-project"], future))
            self.assertEqual(result["status"], status)
        for payload, deadline, status in [("zlib:!", future, 400), ("zlib:!", 1, 408)]:
            result = json.loads(await self.transport.request(
                "/v1/compress", payload, HEADERS["x-headroom-project"], deadline))
            self.assertEqual(result["status"], status)
        self.assertEqual(self.calls, 1)

    async def test_private_deadline_cleans_up_worker(self):
        entered = asyncio.Event()

        async def blocked(_):
            try:
                entered.set()
                await asyncio.Event().wait()
            finally:
                self.cleaned.set()

        transport = PrivateTransport(create_service(blocked))
        result = json.loads(await transport.request(
            "/v1/compress", json.dumps(BODY), HEADERS["x-headroom-project"],
            int(time.time() * 1000) + 100))
        self.assertEqual(result["status"], 504)
        self.assertTrue(self.cleaned.is_set())

    async def test_private_cancellation_cleans_up_worker(self):
        entered = asyncio.Event()

        async def blocked(_):
            try:
                entered.set()
                await asyncio.Event().wait()
            finally:
                self.cleaned.set()

        transport = PrivateTransport(create_service(blocked))
        task = asyncio.create_task(transport.request(
            "/v1/compress", json.dumps(BODY), HEADERS["x-headroom-project"],
            int(time.time() * 1000) + 5_000))
        await asyncio.wait_for(entered.wait(), 2)
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task
        self.assertTrue(self.cleaned.is_set())

    @unittest.skipUnless(os.environ.get("HEADROOM_REAL_TEST") == "1", "opt-in official package integration")
    async def test_official_headroom_fidelity(self):
        text = json.dumps([{"id": i, "status": "healthy", "target_fact": "KEEP-7391", "region": "east"} for i in range(150)])
        body = {**BODY, "messages": [{**BODY["messages"][0], "content": text}]}
        result = json.loads(await isolated_compress(json.dumps(body).encode()))
        self.assertIn("KEEP-7391", result["messages"][0]["content"])
        self.assertLess(result["tokens_after"], result["tokens_before"])
        self.assertFalse(result.get("ccr_hashes"))


class GPUWorkerTests(unittest.IsolatedAsyncioTestCase):
    async def test_modal_deploys_one_private_l4_class_with_scale_controls(self):
        modal = MagicMock()
        modal.is_local.return_value = False
        modal.App.return_value.cls.side_effect = lambda **kwargs: lambda cls: cls
        modal.concurrent.side_effect = lambda **kwargs: lambda fn: fn
        modal.method.side_effect = lambda **kwargs: lambda fn: fn
        modal.enter.side_effect = lambda **kwargs: lambda fn: fn
        modal.exit.side_effect = lambda **kwargs: lambda fn: fn
        source = Path(__file__).with_name("app.py").read_text()
        namespace = {"__file__": "/root/app.py", "__name__": "app"}
        with patch.dict(sys.modules, modal=modal):
            exec(compile(source, "/root/app.py", "exec"), namespace)
        modal.Image.from_registry.assert_called_once_with(namespace["REGISTRY_IMAGE"], secret=namespace["registry_secret"])
        self.assertRegex(namespace["REGISTRY_IMAGE"], r"^docker\.io/anshulnoori/headroom@sha256:[0-9a-f]{64}$")
        modal.App.return_value.cls.assert_called_once()
        options = modal.App.return_value.cls.call_args.kwargs
        self.assertEqual(options["gpu"], "L4")
        self.assertEqual(options["region"], "us")
        self.assertEqual(options["timeout"], 90)
        self.assertEqual(options["max_containers"], 1)
        self.assertEqual(options["min_containers"], 0)
        self.assertEqual(options["buffer_containers"], 0)
        self.assertEqual(options["scaledown_window"], 30)
        self.assertEqual(options["retries"], 0)
        self.assertTrue(options["enable_memory_snapshot"])
        self.assertEqual(options["experimental_options"], {"enable_gpu_snapshot": True})
        self.assertEqual(options["volumes"], {"/opt/headroom-models": modal.Volume.from_name.return_value.read_only.return_value})
        self.assertEqual([call.kwargs for call in modal.enter.call_args_list], [{"snap": True}, {"snap": False}])
        self.assertEqual(modal.concurrent.call_args.kwargs, {"max_inputs": 1})
        modal.asgi_app.assert_not_called()
        self.assertNotIn("web_endpoint", source)
        self.assertNotIn("web_server", source)
        self.assertIn("class Headroom", source)

        worker = AsyncMock()
        def validate_warmup(raw):
            from service import validate
            if json.loads(raw) != {"operation": "snapshot"}:
                validate(json.loads(raw))
            return b'{}'
        worker.side_effect = validate_warmup
        with patch("gpu.GPUCompressor", return_value=worker), patch("service.create_service") as service, \
                patch("modal_private.PrivateTransport") as transport, patch("builtins.print"), \
                patch.object(Path, "exists", return_value=True):
            instance = namespace["Headroom"]()
            await instance.load()
            worker.warmup.assert_awaited_once()
            self.assertEqual(worker.await_count, 2)
            self.assertEqual(worker.await_args_list[-1].args, (b'{"operation":"snapshot"}',))
            service.assert_not_called()
            transport.assert_not_called()
            await instance.restore()
            service.assert_called_once()
            transport.assert_called_once()
            await instance.close()
            worker.close.assert_awaited_once()

    def test_snapshot_cleanup_waits_and_preserves_live_weights(self):
        from gpu_worker import prepare_snapshot
        torch = MagicMock()
        torch.cuda.memory_reserved.side_effect = [900, 600]
        torch.cuda.memory_allocated.return_value = 550
        canary = MagicMock()
        canary.is_alive.return_value = False
        compressor = SimpleNamespace(_canary_thread=canary)
        with patch.dict(sys.modules, torch=torch), patch("gpu_worker.gc.collect") as collect:
            result = prepare_snapshot(compressor)
            self.assertEqual(result, {"cuda_reserved_before": 900, "cuda_reserved_after": 600, "cuda_allocated": 550})
            canary.join.assert_called_once_with(timeout=5)
            collect.assert_called_once()
            torch.cuda.empty_cache.assert_called_once()
            self.assertEqual(torch.cuda.synchronize.call_count, 2)
            canary.is_alive.return_value = True
            with self.assertRaisesRegex(RuntimeError, "startup probe"):
                prepare_snapshot(compressor)
            self.assertEqual(torch.cuda.empty_cache.call_count, 1)

    def test_loader_patch_rejects_changed_source(self):
        from optimize_loader import optimize
        with self.assertRaisesRegex(ValueError, "loader changed"):
            optimize(b"unexpected source")

    def test_registry_credential_is_only_used_for_image_import(self):
        modal = MagicMock()
        modal.is_local.return_value = True
        modal.Secret.from_name.side_effect = lambda name: name
        path = Path(__file__).with_name("app.py")
        with patch.dict(sys.modules, modal=modal), patch.dict(os.environ, {}, clear=True):
            exec(compile(path.read_text(), str(path), "exec"), {"__file__": str(path), "__name__": "app"})
        self.assertEqual(modal.Image.from_registry.call_args.kwargs["secret"], "headroom-dockerhub")
        self.assertEqual(modal.App.return_value.cls.call_args.kwargs["secrets"], ["bifrost-headroom"])
        modal.Secret.from_dict.assert_not_called()

    def test_l4_rate_ignores_obsolete_t4_override(self):
        path = Path(__file__).with_name("app.py")
        for variable in ["HEADROOM_DEPLOY_GPU", "HEADROOM_ACCELERATOR"]:
            modal = MagicMock()
            namespace = {"__file__": str(path), "__name__": "app"}
            with patch.dict(sys.modules, modal=modal), patch.dict(os.environ, {variable: "T4"}, clear=True):
                exec(compile(path.read_text(), str(path), "exec"), namespace)
            self.assertEqual(modal.App.return_value.cls.call_args.kwargs["gpu"], "L4")
            self.assertAlmostEqual(namespace["COST_PER_SECOND"], 0.000295642)

    async def test_http_benchmark_requires_edge_auth_and_reuses_service(self):
        modal = MagicMock()
        modal.App.return_value.cls.side_effect = lambda **kwargs: lambda cls: cls
        modal.concurrent.side_effect = lambda **kwargs: lambda cls: cls
        modal.asgi_app.side_effect = lambda **kwargs: lambda fn: fn
        path = Path(__file__).with_name("app.py")
        namespace = {"__file__": str(path), "__name__": "app"}
        with patch.dict(sys.modules, modal=modal), patch.dict(os.environ, HEADROOM_HTTP_BENCHMARK="1", HEADROOM_PROXY_TOKEN="internal-test"):
            exec(compile(path.read_text(), str(path), "exec"), namespace)
            modal.asgi_app.assert_called_once_with(requires_proxy_auth=True)
            worker = namespace["Headroom"]()
            service = AsyncMock()
            worker.transport = SimpleNamespace(service=service)
            scope = {"type": "http", "headers": [(b"x-headroom-proxy-token", b"external"), (b"x-headroom-deadline-ms", b"1")]}
            await worker.http()(scope, None, None)
            forwarded = service.call_args.args[0]
            self.assertEqual(forwarded["headers"], [(b"x-headroom-deadline-ms", b"1"), (b"x-headroom-proxy-token", b"internal-test")])
            self.assertEqual(scope["headers"][0][1], b"external")

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

        result = await compress_until_disconnect(SimpleNamespace(receive=disconnected, headers={}), b"{}", blocked)
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

    def test_invalid_embedding_frame_does_not_kill_healthy_worker(self):
        class Encoder:
            def __call__(self, texts):
                if len(texts[0]) == 22 * 1024:
                    raise EmbeddingInputError("embedding input exceeds the model context")
                return [[1.0] + [0.0] * 383]

        requests = [
            {"operation": "embed", "input": ["x" * (22 * 1024)]},
            {"operation": "embed", "input": ["valid"]},
        ]
        frames = []
        for request in requests:
            raw = json.dumps(request).encode()
            frames.append(struct.pack("!I", len(raw)) + raw)
        output = io.BytesIO()

        serve(io.BytesIO(b"".join(frames)), output, MagicMock(), Encoder())

        output.seek(5)  # readiness marker
        replies = []
        for _ in requests:
            size = struct.unpack("!I", output.read(4))[0]
            replies.append(output.read(size))
        self.assertEqual(replies[0], b'{"error":"invalid embedding input"}')
        self.assertEqual(json.loads(replies[1])["vectors"], [[1.0] + [0.0] * 383])

    def test_embedding_runtime_failure_escapes_without_partial_frame(self):
        encoder = MagicMock(side_effect=RuntimeError("model failed"))
        raw = json.dumps({"operation": "embed", "input": ["valid"]}).encode()
        output = io.BytesIO()

        with self.assertRaisesRegex(RuntimeError, "model failed"):
            serve(io.BytesIO(struct.pack("!I", len(raw)) + raw), output, MagicMock(), encoder)

        self.assertEqual(output.getvalue(), b"CUDA\n")

    def test_attention_and_precision_apply_before_canary_to_entire_scorer(self):
        model = MagicMock()
        events = []
        model.encoder.set_attn_implementation.side_effect = lambda value: events.append(("attention", value))
        model.to.side_effect = lambda **kw: events.append(("dtype", kw["dtype"]))
        compressor = SimpleNamespace(config=SimpleNamespace(model_id="pinned"),
                                     preload=lambda **kw: events.append(("preload", kw)) or "pytorch")
        module = SimpleNamespace(KompressCompressor=lambda config: compressor,
                                 KompressConfig=lambda **kw: kw,
                                 _load_kompress=MagicMock(return_value=(model, None, "pytorch")))
        torch = SimpleNamespace(cuda=SimpleNamespace(is_available=lambda: True), bfloat16="bf16")
        with patch.dict(sys.modules, {"torch": torch, "headroom.transforms.kompress_compressor": module}), \
             patch.dict(os.environ, HEADROOM_ATTENTION="sdpa", HEADROOM_PRECISION="bfloat16"):
            self.assertIs(load_compressor(), compressor)
            self.assertEqual(events, [("attention", "sdpa"), ("dtype", "bf16"), ("preload", {"allow_download": False})])
            module._load_kompress.assert_called_once_with("pinned", "cuda", allow_download=False)
            events.clear()
            with patch.dict(os.environ, HEADROOM_PRECISION="default"):
                load_compressor()
            self.assertEqual(events, [("attention", "sdpa"), ("preload", {"allow_download": False})])
            with patch.dict(os.environ, HEADROOM_ATTENTION="typo"):
                with self.assertRaisesRegex(ValueError, "configuration"):
                    load_compressor()

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
            with self.assertRaises(RuntimeError):
                load_compressor("cpu")  # Never silently run PyTorch on CPU.
            with patch.object(Compressor, "preload", return_value="onnx"):
                load_compressor("cpu")
            self.assertEqual(selected[-1], {"device": "cpu", "enable_ccr": False})

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
            self.assertEqual(env["HEADROOM_ATTENTION"], "sdpa")
            self.assertEqual(env["HEADROOM_PRECISION"], "float16")
            self.assertEqual(env["HEADROOM_KOMPRESS_BACKEND"], "onnx_cpu" if worker.device == "cpu" else "pytorch")
            if worker.device == "cpu":
                self.assertEqual(env["HEADROOM_KOMPRESS_ONNX_FILENAME"], "onnx/kompress-int8-wo.onnx")
            self.assertEqual(env["HF_HUB_OFFLINE"], "1")
            return SimpleNamespace(stdout=SimpleNamespace(readexactly=AsyncMock(return_value=f"{worker.device.upper():4}\n".encode())),
                                   returncode=0, wait=AsyncMock())
        worker = GPUCompressor()
        with patch.dict(os.environ, MODAL_TOKEN_SECRET="synthetic", HEADROOM_PROXY_TOKEN="synthetic",
                        HEADROOM_ATTENTION="sdpa", HEADROOM_PRECISION="float16"), \
                patch("gpu.asyncio.create_subprocess_exec", side_effect=spawn):
            await worker.warmup()
            await worker.close()
            worker = GPUCompressor(device="cpu")
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
            cls.created_role = connection.execute("SELECT 1 FROM pg_roles WHERE rolname = 'headroom'").fetchone() is None
            if cls.created_role:
                connection.execute("CREATE ROLE headroom")
            connection.execute(sql.SQL("CREATE DATABASE {} OWNER headroom").format(sql.Identifier(cls.database)))
        cls.dsn = cls.admin + " dbname=" + cls.database
        with psycopg.connect(cls.dsn, autocommit=True) as connection:
            connection.execute("SET ROLE headroom")
            connection.execute(Path(__file__).resolve().parents[2].joinpath("neon/headroom-state.sql").read_text())

        def restricted(dsn, **kwargs):
            connection = psycopg.connect(dsn, **kwargs)
            connection.execute("SET ROLE headroom")
            return connection

        cls.store = StateStore(cls.dsn, b"x" * 32, connect=restricted)

    @classmethod
    def tearDownClass(cls):
        import psycopg
        from psycopg import sql
        with psycopg.connect(cls.admin, autocommit=True) as connection:
            connection.execute(sql.SQL("DROP DATABASE {} ").format(sql.Identifier(cls.database)))
            if cls.created_role:
                connection.execute("DROP ROLE headroom")

    def setUp(self):
        self.scope = uuid.uuid4().hex * 2

    def test_application_role_owns_schema_and_can_migrate(self):
        with self.store.connect(self.dsn) as connection:
            self.assertEqual(connection.execute("SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'headroom'").fetchone()[0], "headroom")
            self.assertEqual(connection.execute("SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'headroom.entries'::regclass").fetchone()[0], "headroom")
            connection.execute("CREATE TABLE headroom.migration_check (id integer)")
            connection.execute("ALTER TABLE headroom.migration_check ADD COLUMN note text")
            connection.execute("DROP TABLE headroom.migration_check")

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
