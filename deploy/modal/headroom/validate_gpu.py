"""Owner-authorized, ephemeral T4 validation. Does not deploy an HTTP endpoint."""
import json
from pathlib import Path
import modal

if modal.is_local():
    from app import gpu_image
    image = gpu_image.add_local_file(Path(__file__).with_name("benchmark.py"), "/root/benchmark.py")
else:
    image = modal.Image.debian_slim()

app = modal.App("headroom-t4-validation")


@app.function(image=image, gpu="T4", cpu=2, memory=4096,
              timeout=180, startup_timeout=120, retries=0,
              min_containers=0, max_containers=1, buffer_containers=0, scaledown_window=30)
async def validate() -> str:
    import asyncio
    import time
    import subprocess
    from gpu import GPUCompressor
    from benchmark import assess_result, cost_summary

    started = time.perf_counter()
    hardware = subprocess.check_output(
        ["nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader"], text=True).strip()
    if "T4" not in hardware:
        raise RuntimeError("unexpected GPU allocation")
    worker = GPUCompressor(device="cuda")
    records = []
    report = {"hardware": hardware, "requested_backend": "cuda-pytorch",
              "onnx_allowed": False, "scope": "remote worker, not gateway end-to-end",
              "production_changed": False}
    try:
        async with asyncio.timeout(120):
            load_started = time.perf_counter()
            # CUDA handshake occurs only after PyTorch preload and CUDA MiniLM forward.
            await worker.warmup()
            report["model_load_ms"] = (time.perf_counter() - load_started) * 1000
            for rows in (20, 200):
                content = json.dumps([{"id": i, "region": "east", "fact": "KEEP-7391", "status": "healthy"} for i in range(rows)])
                body = {"model": "gpt-4.1", "messages": [{"role": "tool", "tool_call_id": "slot-0", "content": content}],
                        "config": {"protect_recent": 0, "compress_user_messages": False},
                        "gateway": {"can_redrive": False, "can_relay_response": False,
                                    "session_affinity": False, "plugin_version": "bifrost-headroom/1"}}
                raw = json.dumps(body).encode()
                for sample in range(3):
                    request_started = time.perf_counter()
                    record = {"input_bytes": len(content.encode()), "sample": sample}
                    try:
                        record.update(assess_result(json.loads(await worker(raw))))
                    except Exception:
                        record["status"] = "failed"
                    record["round_trip_ms"] = (time.perf_counter() - request_started) * 1000
                    records.append(record)
                    if record["status"] == "failed":
                        raise RuntimeError("worker failed; stopping paid validation")
    except Exception as exc:
        report["failure_type"] = type(exc).__name__  # Never return payload-bearing exceptions.
    finally:
        await worker.close()
    elapsed = time.perf_counter() - started
    report.update(records=records, function_elapsed_seconds=elapsed,
                  cost_estimate=cost_summary(records, "t4", elapsed + 30),
                  cost_basis="function wall time plus 30s idle allowance; excludes pre-function startup/builds; not an invoice",
                  quality_claim="single synthetic sentinel only")
    return json.dumps(report, indent=2)
