"""Private deployment: modal deploy deploy/modal/headroom/app.py."""
import json
import os
from pathlib import Path
import time

import modal

ROOT = Path(__file__).resolve().parents[3] if modal.is_local() else Path("/root")
ACCELERATOR = "L4"
HTTP_BENCHMARK = os.environ.get("HEADROOM_HTTP_BENCHMARK") == "1"
ATTENTION = os.environ.get("HEADROOM_ATTENTION", "sdpa")
PRECISION = os.environ.get("HEADROOM_PRECISION", "float16")
if ATTENTION not in {"eager", "sdpa"} or PRECISION not in {"default", "float16", "bfloat16"}:
    raise ValueError("invalid inference configuration")
# Modal base rates retrieved 2026-09-26, https://modal.com/pricing.
# Include approved broad-US placement; exclude storage, image builds and credits.
COST_PER_SECOND = (0.000222 + 2 * 0.0000131 + 4 * 0.00000222) * 1.15

REGISTRY_IMAGE = "docker.io/anshulnoori/headroom@sha256:388fb738417bd91f325c1a67f87d3224c477c4b518dea54960c103b08b45d331"
# Read-only registry credentials belong to the image importer, not inference.
registry_secret = modal.Secret.from_name("headroom-dockerhub")
base_image = modal.Image.from_registry(REGISTRY_IMAGE, secret=registry_secret)
image = (
    base_image
    .env({"HEADROOM_OFFLINE": "1", "HEADROOM_BEACON": "off", "HEADROOM_TELEMETRY": "off",
          "HEADROOM_LOG_PAYLOAD_PREVIEW": "0", "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1",
          "HEADROOM_HTTP_BENCHMARK": "1" if HTTP_BENCHMARK else "0",
          "HEADROOM_ATTENTION": ATTENTION, "HEADROOM_PRECISION": PRECISION,
          "HEADROOM_ACCELERATOR": ACCELERATOR, "HEADROOM_KOMPRESS_BACKEND": "pytorch"})
    .add_local_file(Path(__file__).with_name("service.py"), "/root/service.py")
    .add_local_file(Path(__file__).with_name("modal_private.py"), "/root/modal_private.py")
    .add_local_file(Path(__file__).with_name("gpu.py"), "/root/gpu.py")
    .add_local_file(Path(__file__).with_name("encoder.py"), "/root/encoder.py")
    .add_local_file(Path(__file__).with_name("gpu_worker.py"), "/root/gpu_worker.py")
)

app = modal.App("bifrost-headroom")
# Immutable release: populate once, then mount read-only. New weights need a new
# Volume name and a new snapshot; never edit files under an existing snapshot.
weights = modal.Volume.from_name("bifrost-headroom-models-v1", create_if_missing=True)


@app.function(image=base_image.add_local_file(Path(__file__).with_name("bake_models.py"), "/root/bake_models.py"),
              volumes={"/opt/headroom-models": weights}, cpu=2, memory=4096,
              timeout=300, retries=0, max_containers=1, min_containers=0)
def prepare_weights():
    from bake_models import bake

    ready = Path("/opt/headroom-models/READY")
    if not ready.exists():
        bake()
        ready.write_text("v1\n")
        weights.commit()
    return "pinned weights ready"


@app.cls(image=image, gpu=ACCELERATOR, region="us", cpu=2, memory=4096, timeout=90, retries=0,
         max_containers=1, min_containers=0, buffer_containers=0, scaledown_window=30,
         enable_memory_snapshot=True, experimental_options={"enable_gpu_snapshot": True},
         volumes={"/opt/headroom-models": weights.read_only()},
         secrets=[modal.Secret.from_name("bifrost-headroom")])
@modal.concurrent(max_inputs=1)
class Headroom:
    @modal.enter(snap=True)
    async def load(self):
        from gpu import GPUCompressor

        if not Path("/opt/headroom-models/READY").exists():
            raise RuntimeError("prepare pinned model Volume before deployment")
        started = time.monotonic()
        self.compressor = GPUCompressor(device="cuda")
        await self.compressor.warmup()
        loaded = time.monotonic()
        # Synthetic input only: initialize inference before capturing the worker.
        # Never capture a real request or an open service/database connection.
        await self.compressor(json.dumps({
            "model": "gpt-4.1",
            "messages": [{"role": "tool", "tool_call_id": "slot-0", "content":
                          "INFO request completed successfully\n" * 500}],
            "config": {"protect_recent": 0, "compress_user_messages": False},
            "gateway": {"can_redrive": False, "can_relay_response": False,
                        "session_affinity": False, "plugin_version": "bifrost-headroom/1"},
        }).encode())
        memory = json.loads(await self.compressor(b'{"operation":"snapshot"}'))
        elapsed = time.monotonic() - started
        print(json.dumps({"kind": "headroom_snapshot_prepare", "configured_accelerator": ACCELERATOR,
                          "memory": memory,
                          "model_load_ms": (loaded - started) * 1000,
                          "warmup_ms": (time.monotonic() - loaded) * 1000,
                          "elapsed_ms": elapsed * 1000,
                          "estimated_active_usd": elapsed * COST_PER_SECOND}), flush=True)

    @modal.enter(snap=False)
    async def restore(self):
        from modal_private import PrivateTransport
        from service import create_service

        started = time.monotonic()
        service = create_service(
            self.compressor,
            policy="cuda-kompress-marker-free-v1",
            embed=self.compressor.embed,
            cost_per_second=COST_PER_SECOND,
        )
        self.transport = PrivateTransport(service)
        elapsed = time.monotonic() - started
        # This measures post-restore setup, not Modal's snapshot restore duration.
        print(json.dumps({"kind": "headroom_service_ready", "configured_accelerator": ACCELERATOR,
                          "attention": ATTENTION, "precision": PRECISION,
                          "region": os.environ.get("MODAL_REGION", "unknown"),
                          "elapsed_ms": elapsed * 1000,
                          "estimated_active_usd": elapsed * COST_PER_SECOND}), flush=True)

    @modal.exit()
    async def close(self):
        await self.compressor.close()

    @modal.method()
    async def request(self, path: str, body: str, scope: str, deadline_ms: int) -> str:
        return await self.transport.request(path, body, scope, deadline_ms)

    if HTTP_BENCHMARK:
        @modal.asgi_app(requires_proxy_auth=True)
        def http(self):
            # Modal authenticates at the edge before admitting any GPU work.
            # Reuse the exact bounded service; replace only its internal token.
            async def authenticated(scope, receive, send):
                if scope["type"] == "http":
                    headers = [(k, v) for k, v in scope["headers"] if k.lower() != b"x-headroom-proxy-token"]
                    headers.append((b"x-headroom-proxy-token", os.environ["HEADROOM_PROXY_TOKEN"].encode()))
                    scope = {**scope, "headers": headers}
                await self.transport.service(scope, receive, send)
            return authenticated
