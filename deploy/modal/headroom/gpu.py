"""One killable CUDA worker per Modal container, serialized by the ASGI facade."""
import asyncio
import os
from pathlib import Path
import struct
import sys
import tempfile

from service import MAX_BODY


class GPUCompressor:
    def __init__(self):
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
            "PATH", "LD_LIBRARY_PATH", "CUDA_VISIBLE_DEVICES", "NVIDIA_VISIBLE_DEVICES"}}
        env.update(HOME=self.scratch.name, TMPDIR=self.scratch.name,
                   HF_HUB_CACHE="/opt/headroom-models", HF_HUB_OFFLINE="1",
                   TRANSFORMERS_OFFLINE="1", HEADROOM_OFFLINE="1",
                   HEADROOM_KOMPRESS_BACKEND="pytorch", HEADROOM_BEACON="off",
                   HEADROOM_TELEMETRY="off", HEADROOM_LOG_PAYLOAD_PREVIEW="0",
                   HF_HUB_DISABLE_TELEMETRY="1", HEADROOM_CCR_BACKEND="memory")
        try:
            self.process = await asyncio.create_subprocess_exec(
                sys.executable, str(Path(__file__).with_name("gpu_worker.py")),
                stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.DEVNULL, cwd=self.scratch.name, env=env)
            if await asyncio.wait_for(self.process.stdout.readexactly(5), 60) != b"CUDA\n":
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
