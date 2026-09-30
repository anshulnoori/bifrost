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

from fastapi import FastAPI, Request, HTTPException
from fastapi.responses import JSONResponse, Response

MAX_BODY = 4 * 1024 * 1024
VERSION = "headroom-ai/0.38.0"
GATEWAY = {"can_redrive": False, "can_relay_response": False,
           "session_affinity": False, "plugin_version": "bifrost-headroom/1"}
CCR_GATEWAY = {**GATEWAY, "can_redrive": True, "plugin_version": "bifrost-headroom/2"}


def validate(body, allow_ccr=False):
    if not isinstance(body, dict) or set(body) != {"model", "messages", "config", "gateway"}:
        raise ValueError()
    if not isinstance(body["model"], str) or not 1 <= len(body["model"]) <= 256:
        raise ValueError()
    config = {"protect_recent": 0, "compress_user_messages": False}
    ccr = allow_ccr and body["config"] == {**config, "ccr": True}
    if not ccr and body["config"] != config:
        raise ValueError()
    if body["gateway"] != (CCR_GATEWAY if ccr else GATEWAY):
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


async def compress_until_disconnect(request, raw, compressor):
    async def disconnected():
        # The complete request body has already been consumed. Read the next
        # ASGI event directly; is_disconnected() can swallow task cancellation.
        while (await request.receive())["type"] != "http.disconnect":
            pass

    deadline = request.headers.get("x-headroom-deadline-ms")
    timeout = min(25, int(deadline) / 1000 - time.time()) if deadline else 25
    if timeout <= 0:
        raise asyncio.TimeoutError()
    work = asyncio.create_task(compressor(raw))
    watcher = asyncio.create_task(disconnected())
    try:
        done, _ = await asyncio.wait({work, watcher}, timeout=timeout, return_when=asyncio.FIRST_COMPLETED)
        if not done:
            raise asyncio.TimeoutError()
        if watcher in done:
            return None
        return await work
    finally:
        for task in (work, watcher):
            if not task.done():
                task.cancel()
        await asyncio.gather(work, watcher, return_exceptions=True)


async def read_json(request):
    if request.headers.get("content-type", "").split(";")[0] != "application/json":
        raise HTTPException(415)
    raw = bytearray()
    async for chunk in request.stream():
        raw.extend(chunk)
        if len(raw) > MAX_BODY:
            raise HTTPException(413)
    try:
        return json.loads(raw)
    except ValueError:
        raise HTTPException(400) from None


def create_service(compressor=isolated_compress, *, policy="isolated-marker-free-cpu-v1", lifespan=None, features=None, embed=None, cost_per_second=0):
    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None, lifespan=lifespan)
    slots = asyncio.Semaphore(1)

    @app.middleware("http")
    async def boundary(request: Request, call_next):
        token = os.environ.get("HEADROOM_PROXY_TOKEN", "")
        provided = request.headers.get("x-headroom-proxy-token", "")
        if len(token) < 32 or not hmac.compare_digest(provided.encode(), token.encode()):
            return Response(status_code=401)
        allowed = {("POST", "/v1/compress"), ("GET", "/health"), ("GET", "/version")}
        if features is not None:
            allowed.add(("POST", "/v1/tools"))
        if embed is not None:
            allowed.update({("POST", "/v1/embeddings"), ("GET", "/v1/models")})
        if (request.method, request.url.path) not in allowed:
            return Response(status_code=404)
        if request.url.query or request.headers.get("content-encoding"):
            return Response(status_code=400)
        deadline = request.headers.get("x-headroom-deadline-ms")
        if deadline is not None:
            if not deadline.isdecimal() or len(deadline) > 16:
                return Response(status_code=400)
            if int(deadline) <= time.time() * 1000:
                return Response(status_code=408)
        started = time.monotonic()
        response = await call_next(request)
        response.headers["cache-control"] = "no-store"
        elapsed = time.monotonic() - started
        # Authenticated routes only. No body, identifiers, credentials or query text.
        # Active request estimate excludes startup, idle time and image builds.
        if cost_per_second:
            print(json.dumps({"kind": "headroom_request_cost", "route": request.url.path,
                              "status": response.status_code, "elapsed_ms": round(elapsed * 1000, 3),
                              "estimated_active_usd": elapsed * cost_per_second,
                              "excludes_startup_idle_builds": True}), flush=True)
        return response

    @app.get("/health")
    async def health():
        return {"status": "ready"}

    @app.get("/version")
    async def version():
        return {"version": VERSION, "policy": policy, "performance": "unmeasured",
                "ccr": features is not None, "memory": features is not None, "embeddings": embed is not None}

    @app.get("/v1/models")
    async def models():
        return {"object": "list", "data": [{"id": "headroom-minilm-v1", "object": "model", "owned_by": "headroom"}]}

    @app.post("/v1/embeddings")
    async def embeddings(request: Request):
        body = await read_json(request)
        if not isinstance(body, dict) or set(body) - {"model", "input", "encoding_format", "dimensions"} or body.get("model") != "headroom-minilm-v1" or body.get("encoding_format", "float") != "float" or body.get("dimensions", 384) != 384:
            return Response(status_code=400)
        texts = body.get("input")
        if isinstance(texts, str):
            texts = [texts]
        if not isinstance(texts, list) or not 1 <= len(texts) <= 32 or any(not isinstance(t, str) or not 1 <= len(t.encode()) <= 32768 for t in texts):
            return Response(status_code=400)
        if slots.locked():
            return Response(status_code=429)
        try:
            async with slots:
                vectors = await compress_until_disconnect(request, texts, embed)
            if vectors is None:
                return Response(status_code=499)
            return {"object": "list", "model": "headroom-minilm-v1",
                    "data": [{"object": "embedding", "index": i, "embedding": vector} for i, vector in enumerate(vectors)]}
        except (ValueError, asyncio.TimeoutError):
            return Response(status_code=502)

    @app.post("/v1/tools")
    async def tools(request: Request):
        scope = request.headers.get("x-headroom-project", "")
        body = await read_json(request)
        if not re.fullmatch(r"[a-f0-9]{64}", scope) or not isinstance(body, dict) or set(body) != {"name", "arguments", "operation_id"} or not isinstance(body["operation_id"], str) or not re.fullmatch(r"[a-f0-9]{64}", body["operation_id"]):
            return Response(status_code=400)
        if slots.locked():
            return Response(status_code=429)
        from state import StateError
        try:
            async def execute(_):
                return await features.tool(scope, body["name"], body["arguments"], body["operation_id"])
            async with slots:
                result = await compress_until_disconnect(request, None, execute)
            if result is None:
                return Response(status_code=499)
            return result
        except (StateError, ValueError, TypeError, KeyError, asyncio.TimeoutError):
            return Response(status_code=502)

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
            validate(body, allow_ccr=features is not None)
        except (ValueError, TypeError):
            return Response(status_code=400)
        if slots.locked():
            return Response(status_code=429, headers={"retry-after": "1"})
        started = time.monotonic()
        try:
            ccr = body["config"].get("ccr", False)
            worker_body = {**body, "gateway": GATEWAY, "config": {"protect_recent": 0, "compress_user_messages": False}}
            async with slots:
                output = await compress_until_disconnect(request, json.dumps(worker_body).encode(), compressor)
            if output is None:
                return Response(status_code=499)
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
            hashes = []
            if ccr:
                from state import StateError
                from features import state_call
                try:
                    async with slots:
                        texts, hashes = await state_call(features.retain, request.headers["x-headroom-project"],
                            [m["content"] for m in body["messages"]], [m["content"] for m in messages])
                except StateError:
                    return Response(status_code=502)
                messages = [{**message, "content": text} for message, text in zip(messages, texts)]
                # Marker overhead is not an upstream tokenizer measurement.
                # Omit savings rather than undercounting the actual request.
                after = before
            # Strip diagnostics or text fields that upstream can add in later releases.
            return JSONResponse({"messages": messages, "tokens_before": before, "tokens_after": after,
                                 "ccr_hashes": hashes, "obligations": []}, headers={
                                     "x-compression-ms": str(round((time.monotonic() - started) * 1000)),
                                     "x-headroom-policy": policy})
        except asyncio.TimeoutError:
            return Response(status_code=504)
        except (ValueError, KeyError, TypeError):
            return Response(status_code=502)

    return app
