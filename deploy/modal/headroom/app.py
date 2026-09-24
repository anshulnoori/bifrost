"""Deployment is owner-operated: modal deploy deploy/modal/headroom/app.py."""
from pathlib import Path
import modal

ROOT = Path(__file__).resolve().parents[3]
image = (
    modal.Image.debian_slim(python_version="3.11")
    .add_local_file(ROOT / "integrations/headroom/requirements.lock", "/tmp/requirements.lock", copy=True)
    .run_commands("python -m pip install --require-hashes -r /tmp/requirements.lock")
    .env({"HEADROOM_OFFLINE": "1", "HEADROOM_BEACON": "off", "HEADROOM_TELEMETRY": "off",
          "HEADROOM_LOG_PAYLOAD_PREVIEW": "0", "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1"})
    .add_local_file(Path(__file__).with_name("service.py"), "/root/service.py")
    .add_local_file(Path(__file__).with_name("compress_once.py"), "/root/compress_once.py")
)
app = modal.App("bifrost-headroom")


@app.function(image=image, cpu=2, memory=2048, timeout=60, max_containers=2,
              min_containers=0, scaledown_window=60,
              secrets=[modal.Secret.from_name("bifrost-headroom")])
@modal.concurrent(max_inputs=1)
@modal.asgi_app(requires_proxy_auth=True)
def service():
    from service import create_service
    return create_service()


# Separate endpoint: CPU remains available for rollback and comparison.
gpu_image = (
    modal.Image.debian_slim(python_version="3.11")
    .add_local_file(Path(__file__).with_name("requirements-gpu.lock"), "/tmp/gpu.lock", copy=True)
    .run_commands("python -m pip install --require-hashes -r /tmp/gpu.lock")
    .add_local_file(Path(__file__).with_name("requirements-state.lock"), "/tmp/state.lock", copy=True)
    .run_commands("python -m pip install --require-hashes -r /tmp/state.lock")
    .add_local_file(Path(__file__).with_name("bake_models.py"), "/tmp/bake_models.py", copy=True)
    .run_commands("python /tmp/bake_models.py", "chmod -R a-w /opt/headroom-models")
    .env({"HEADROOM_OFFLINE": "1", "HEADROOM_BEACON": "off", "HEADROOM_TELEMETRY": "off",
          "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1", "HEADROOM_KOMPRESS_BACKEND": "pytorch"})
    .add_local_file(Path(__file__).with_name("service.py"), "/root/service.py")
    .add_local_file(Path(__file__).with_name("gpu.py"), "/root/gpu.py")
    .add_local_file(Path(__file__).with_name("encoder.py"), "/root/encoder.py")
    .add_local_file(Path(__file__).with_name("gpu_worker.py"), "/root/gpu_worker.py")
    .add_local_file(Path(__file__).with_name("state.py"), "/root/state.py")
    .add_local_file(Path(__file__).with_name("features.py"), "/root/features.py")
)


@app.function(image=gpu_image, gpu="L4", cpu=2, memory=4096, timeout=90,
              max_containers=2, min_containers=0, scaledown_window=60,
              secrets=[modal.Secret.from_name("bifrost-headroom")])
@modal.concurrent(max_inputs=1)
@modal.asgi_app(requires_proxy_auth=True)
def gpu_service():
    from contextlib import asynccontextmanager
    from gpu import GPUCompressor
    from service import create_service
    from state import StateStore
    from features import StateFeatures

    compressor = GPUCompressor()
    features = StateFeatures(StateStore.from_environment(), compressor.embed)

    @asynccontextmanager
    async def lifespan(_):
        try:
            await compressor.warmup()  # CUDA forward-pass check precedes readiness.
            yield
        finally:
            await compressor.close()

    return create_service(compressor, policy="cuda-kompress-v2", lifespan=lifespan, embed=compressor.embed, features=features)
