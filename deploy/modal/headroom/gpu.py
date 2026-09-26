"""One killable CUDA worker per Modal container, serialized by the ASGI facade."""
import asyncio
import json
import math
import os
from pathlib import Path
import struct
import sys
import tempfile

from service import MAX_BODY


class GPUCompressor:
    def __init__(self, device="cuda"):
        if device not in {"cuda", "cpu"}:
            raise ValueError("invalid worker device")
        self.device = device
        self.process = None
        self.scratch = None

    async def close(self):
        process, self.process = self.process, None
        if process is not None:
            if process.returncode is None:
                try:
                    process.kill()
                except ProcessLookupError:
                    pass
            await process.wait()
        if self.scratch is not None:
            self.scratch.cleanup()
            self.scratch = None

    async def warmup(self):
        if self.process is not None:
            return
        self.scratch = tempfile.TemporaryDirectory(prefix="headroom-cuda-")
        env = {k: v for k, v in os.environ.items() if k in {
            "PATH", "LD_LIBRARY_PATH", "CUDA_VISIBLE_DEVICES", "NVIDIA_VISIBLE_DEVICES",
            "HEADROOM_ATTENTION", "HEADROOM_PRECISION"}}
        env.update(HOME=self.scratch.name, TMPDIR=self.scratch.name,
                   HF_HUB_CACHE="/opt/headroom-models", HF_HUB_OFFLINE="1",
                   TRANSFORMERS_OFFLINE="1", HEADROOM_OFFLINE="1",
                   HEADROOM_KOMPRESS_BACKEND="onnx_cpu" if self.device == "cpu" else "pytorch", HEADROOM_BEACON="off",
                   HEADROOM_TELEMETRY="off", HEADROOM_LOG_PAYLOAD_PREVIEW="0",
                   HF_HUB_DISABLE_TELEMETRY="1", HEADROOM_CCR_BACKEND="memory",
                   HEADROOM_WORKER_DEVICE=self.device)
        if self.device == "cpu":
            env["HEADROOM_KOMPRESS_ONNX_FILENAME"] = "onnx/kompress-int8-wo.onnx"
        try:
            self.process = await asyncio.create_subprocess_exec(
                sys.executable, str(Path(__file__).with_name("gpu_worker.py")),
                stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.DEVNULL, cwd=self.scratch.name, env=env)
            if await asyncio.wait_for(self.process.stdout.readexactly(5), 60) != f"{self.device.upper():4}\n".encode():
                raise ValueError()
        except BaseException:
            await self.close()
            raise

    async def __call__(self, raw):
        if not 0 < len(raw) <= MAX_BODY:
            raise ValueError()
        try:
            async with asyncio.timeout(25):
                await self.warmup()
                self.process.stdin.write(struct.pack("!I", len(raw)) + raw)
                await self.process.stdin.drain()
                size = struct.unpack("!I", await self.process.stdout.readexactly(4))[0]
                if not 0 < size <= MAX_BODY:
                    raise ValueError()
                return await self.process.stdout.readexactly(size)
        except asyncio.CancelledError:
            await self.close()
            raise
        except Exception:
            await self.close()
            # The caller gets a generic failure and Bifrost retains the original input.
            raise ValueError("CUDA worker failed") from None

    async def embed(self, texts):
        from encoder import DIMENSIONS
        output = json.loads(await self(json.dumps({"operation": "embed", "input": texts}).encode()))
        if output == {"error": "invalid embedding input"}:
            raise ValueError("invalid embedding input")
        vectors = output.get("vectors")
        if not isinstance(vectors, list) or len(vectors) != len(texts):
            raise ValueError("invalid embedding output")
        for vector in vectors:
            if not isinstance(vector, list) or len(vector) != DIMENSIONS or any(type(x) not in {int, float} or not math.isfinite(x) for x in vector):
                raise ValueError("invalid embedding output")
            if abs(sum(x * x for x in vector) - 1) > 0.01:
                raise ValueError("invalid embedding normalization")
        return vectors
