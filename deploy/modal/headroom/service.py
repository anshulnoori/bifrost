"""Narrow authenticated ASGI facade for the official Headroom implementation."""
import asyncio
import hmac
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import time

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, Response

MAX_BODY = 4 * 1024 * 1024
VERSION = "headroom-ai/0.38.0; policy=isolated-marker-free-cpu-v1"


def validate(body):
    if not isinstance(body, dict) or set(body) != {"model", "messages", "config", "gateway"}:
        raise ValueError()
    if not isinstance(body["model"], str) or not 1 <= len(body["model"]) <= 256:
        raise ValueError()
    if body["config"] != {"protect_recent": 0, "compress_user_messages": False}:
        raise ValueError()
    if body["gateway"] != {"can_redrive": False, "can_relay_response": False,
                           "session_affinity": False, "plugin_version": "bifrost-headroom/1"}:
        raise ValueError()
    messages = body["messages"]
    if not isinstance(messages, list) or not 1 <= len(messages) <= 256:
        raise ValueError()
    for i, msg in enumerate(messages):
        if not isinstance(msg, dict) or set(msg) != {"role", "tool_call_id", "content"}:
            raise ValueError()
        if msg["role"] != "tool" or msg["tool_call_id"] != f"slot-{i}" or not isinstance(msg["content"], str):
            raise ValueError()
        if not msg["content"] or "<<ccr:" in msg["content"] or "headroom_retrieve" in msg["content"]:
            raise ValueError()


async def isolated_compress(raw):
    # Child receives no Modal credentials and no provider credentials.
    with tempfile.TemporaryDirectory(prefix="headroom-request-") as scratch:
        child_env = {k: v for k, v in os.environ.items() if k in {
            "PATH", "LD_LIBRARY_PATH", "PYTHONPATH", "CUDA_VISIBLE_DEVICES"}}
        child_env.update(HOME=scratch, TMPDIR=scratch, HEADROOM_OFFLINE="1",
                         HEADROOM_BEACON="off", HEADROOM_TELEMETRY="off")
        process = await asyncio.create_subprocess_exec(
            sys.executable, str(Path(__file__).with_name("compress_once.py")),
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL, cwd=scratch, env=child_env,
        )
        try:
            output, _ = await asyncio.wait_for(process.communicate(raw), timeout=25)
            if process.returncode != 0 or len(output) > MAX_BODY:
                raise ValueError()
            return output
        finally:
            if process.returncode is None:
                process.kill()
                await process.wait()


def create_service(compressor=isolated_compress):
    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
    slots = asyncio.Semaphore(1)

    @app.middleware("http")
    async def boundary(request: Request, call_next):
        token = os.environ.get("HEADROOM_PROXY_TOKEN", "")
        provided = request.headers.get("x-headroom-proxy-token", "")
        if len(token) < 32 or not hmac.compare_digest(provided.encode(), token.encode()):
            return Response(status_code=401)
        if (request.method, request.url.path) not in {("POST", "/v1/compress"), ("GET", "/health"), ("GET", "/version")}:
            return Response(status_code=404)
        if request.url.query or request.headers.get("content-encoding"):
            return Response(status_code=400)
        response = await call_next(request)
        response.headers["cache-control"] = "no-store"
        return response

    @app.get("/health")
    async def health():
        return {"status": "ready"}

    @app.get("/version")
    async def version():
        return {"version": VERSION, "gpu_policy": "disabled-unmeasured"}

    @app.post("/v1/compress")
    async def compress(request: Request):
        if request.headers.get("content-type", "").split(";")[0] != "application/json":
            return Response(status_code=415)
        if not re.fullmatch(r"[a-f0-9]{64}", request.headers.get("x-headroom-project", "")):
            return Response(status_code=400)
        length = request.headers.get("content-length")
        if length is not None and (not length.isdecimal() or int(length) > MAX_BODY):
            return Response(status_code=413)
        raw = bytearray()
        async for chunk in request.stream():
            raw.extend(chunk)
            if len(raw) > MAX_BODY:
                return Response(status_code=413)
        try:
            body = json.loads(raw)
            validate(body)
        except (ValueError, TypeError):
            return Response(status_code=400)
        if slots.locked():
            return Response(status_code=429, headers={"retry-after": "1"})
        started = time.monotonic()
        try:
            async with slots:
                output = await compressor(bytes(raw))
            data = json.loads(output)
            if data.get("ccr_hashes") or data.get("obligations"):
                raise ValueError()
            messages = data["messages"]
            if len(messages) != len(body["messages"]):
                raise ValueError()
            for old, new in zip(body["messages"], messages):
                text = new.get("content")
                if not isinstance(text, str) or not text or len(text.encode()) > len(old["content"].encode()) or "<<ccr:" in text or "headroom_retrieve" in text:
                    raise ValueError()
                if {**new, "content": old["content"]} != old:
                    raise ValueError()
            before, after = data["tokens_before"], data["tokens_after"]
            if type(before) is not int or type(after) is not int or not 0 <= after <= before:
                raise ValueError()
            # Strip diagnostics or text fields that upstream can add in later releases.
            return JSONResponse({"messages": messages, "tokens_before": before, "tokens_after": after,
                                 "ccr_hashes": [], "obligations": []}, headers={
                                     "x-compression-ms": str(round((time.monotonic() - started) * 1000)),
                                     "x-headroom-policy": "isolated-marker-free-cpu-v1"})
        except asyncio.TimeoutError:
            return Response(status_code=504)
        except (ValueError, KeyError, TypeError):
            return Response(status_code=502)

    return app
