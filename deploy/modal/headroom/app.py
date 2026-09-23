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
