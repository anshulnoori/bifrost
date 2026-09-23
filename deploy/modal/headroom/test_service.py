import asyncio
import json
import os
import unittest
from unittest.mock import patch
import httpx
from service import create_service, isolated_compress, MAX_BODY

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


if __name__ == "__main__":
    unittest.main()
