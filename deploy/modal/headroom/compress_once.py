"""One request per process: no learned state can cross tenant boundaries."""
import asyncio
import contextlib
import json
import logging
import os
import sys


async def compress(payload):
    # Set before importing Headroom. No model downloads or telemetry at runtime.
    os.environ.update(HEADROOM_OFFLINE="1", HF_HUB_OFFLINE="1",
                      TRANSFORMERS_OFFLINE="1", HEADROOM_BEACON="off",
                      HEADROOM_TELEMETRY="off", HEADROOM_LOG_PAYLOAD_PREVIEW="0",
                      HEADROOM_CCR_BACKEND="memory")
    logging.disable(logging.CRITICAL)
    from headroom.proxy.models import ProxyConfig
    from headroom.proxy.server import HeadroomProxy
    from starlette.requests import Request

    proxy = HeadroomProxy(ProxyConfig(
        stateless=True, offline=True, cache_enabled=False, memory_enabled=False,
        lossless=False, disable_kompress=True, disable_kompress_fallback=True,
        ccr_inject_tool=False, ccr_inject_marker=False, ccr_handle_responses=False,
        ccr_context_tracking=False, ccr_proactive_expansion=False,
        discover_pipeline_extensions=False, periodic_toin_stats_enabled=False,
    ))
    # Official HTTP handler and its marker-free pipeline, not a new compressor.
    sent = False

    async def receive():
        nonlocal sent
        if sent:
            return {"type": "http.disconnect"}
        sent = True
        return {"type": "http.request", "body": payload, "more_body": False}

    request = Request({"type": "http", "method": "POST", "path": "/v1/compress",
                       "headers": [(b"content-type", b"application/json")],
                       "client": ("127.0.0.1", 1), "scheme": "http",
                       "server": ("127.0.0.1", 8787), "query_string": b""}, receive)
    response = await proxy.handle_compress(request)
    if response.status_code != 200:
        raise ValueError("compression failed")
    return response.body


if __name__ == "__main__":
    payload = sys.stdin.buffer.read(4 * 1024 * 1024 + 1)
    if len(payload) > 4 * 1024 * 1024:
        sys.exit(2)
    try:
        with contextlib.redirect_stdout(sys.stderr):
            result = asyncio.run(compress(payload))
        sys.stdout.buffer.write(result)
    except Exception:
        # Never emit upstream exceptions: they can contain prompt text.
        sys.exit(1)
