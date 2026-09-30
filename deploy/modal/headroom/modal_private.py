"""In-process private transport for the Headroom ASGI service."""
import base64
import binascii
import json
import os
import time
import zlib

import httpx

from service import MAX_BODY


def envelope(status, body=""):
    return json.dumps({"status": status, "body": body}, separators=(",", ":"))


class PrivateTransport:
    def __init__(self, service):
        self.service = service

    async def request(self, path: str, body: str, scope: str, deadline_ms: int) -> str:
        if type(path) is not str:
            return envelope(400)
        if path not in {"/v1/compress", "/v1/embeddings"}:
            return envelope(404)
        if type(body) is not str or type(scope) is not str or type(deadline_ms) is not int:
            return envelope(400)
        try:
            raw = body.encode("utf-8")
        except UnicodeEncodeError:
            return envelope(400)
        if len(raw) > MAX_BODY:
            return envelope(413)
        remaining = deadline_ms / 1000 - time.time()
        if remaining <= 0:
            return envelope(408)
        if body.startswith("zlib:"):
            try:
                decoder = zlib.decompressobj()
                raw = decoder.decompress(base64.b64decode(raw[5:], validate=True), MAX_BODY + 1)
                if len(raw) > MAX_BODY or decoder.unconsumed_tail:
                    return envelope(413)
                if not decoder.eof or decoder.unused_data:
                    return envelope(400)
            except (binascii.Error, zlib.error):
                return envelope(400)

        headers = {
            "content-type": "application/json",
            "x-headroom-proxy-token": os.environ.get("HEADROOM_PROXY_TOKEN", ""),
            "x-headroom-project": scope,
            "x-headroom-deadline-ms": str(deadline_ms),
        }
        # The ASGI service clamps execution to this deadline and its existing
        # 25-second cap, and owns cancellation/worker cleanup.
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=self.service),
            base_url="http://headroom.internal",
        ) as client:
            response = await client.post(path, headers=headers, content=raw)
        reply = envelope(response.status_code, response.text)
        # Only clients using the packed request protocol accept packed replies.
        if body.startswith("zlib:") and len(reply) >= 4096:
            packed = "zlib:" + base64.b64encode(zlib.compress(reply.encode(), level=1)).decode()
            if len(packed) < len(reply):
                return packed
        return reply
