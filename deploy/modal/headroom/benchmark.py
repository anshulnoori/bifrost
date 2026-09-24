"""Synthetic, body-free results. Live mode requires explicit --live authorization."""
import argparse
import asyncio
import json
import os
import statistics
import time
import httpx
from service import isolated_compress


async def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--samples", type=int, default=3)
    parser.add_argument("--backend", choices=["cpu", "cuda"], default="cpu")
    args = parser.parse_args()
    if not 1 <= args.samples <= 20:
        parser.error("samples must be 1..20")
    url = os.environ.get("HEADROOM_BENCH_URL", "")
    if args.live and not url.startswith("https://"):
        parser.error("set HEADROOM_BENCH_URL to the authorized HTTPS origin")
    if args.backend == "cuda" and not args.live:
        parser.error("CUDA measurement requires the explicitly authorized Modal endpoint")
    records = []
    async with httpx.AsyncClient(timeout=60, follow_redirects=False, trust_env=False) as client:
        for size in [1024, 16384, 131072]:
            content = json.dumps([{"id": i, "region": "east", "fact": "KEEP-7391", "status": "healthy"} for i in range(max(1, size // 80))])
            body = {"model": "gpt-4.1", "messages": [{"role": "tool", "tool_call_id": "slot-0", "content": content}],
                    "config": {"protect_recent": 0, "compress_user_messages": False},
                    "gateway": {"can_redrive": False, "can_relay_response": False, "session_affinity": False, "plugin_version": "bifrost-headroom/1"}}
            for sample in range(args.samples):
                started = time.perf_counter()
                record = {"target_bytes": size, "input_bytes": len(content.encode()), "sample": sample,
                          "environment": "modal" if args.live else "orb-cpu", "backend_requested": args.backend,
                          "process_state": "unknown-modal-placement" if args.live else "fresh-process"}
                try:
                    if args.live:
                        response = await client.post(url + "/v1/compress", json=body, headers={
                            "Modal-Key": os.environ["HEADROOM_MODAL_KEY"], "Modal-Secret": os.environ["HEADROOM_MODAL_SECRET"],
                            "X-Headroom-Proxy-Token": os.environ["HEADROOM_PROXY_TOKEN"], "X-Headroom-Project": "a" * 64})
                        response.raise_for_status()
                        result = response.json()
                        record["service_ms"] = response.headers.get("x-compression-ms")
                        record["policy"] = response.headers.get("x-headroom-policy")
                        expected = "cuda-kompress-marker-free-v1" if args.backend == "cuda" else "isolated-marker-free-cpu-v1"
                        if record["policy"] != expected:
                            raise ValueError("unexpected backend policy")
                    else:
                        result = json.loads(await isolated_compress(json.dumps(body).encode()))
                    record.update(tokens_before_estimated=result["tokens_before"], tokens_after_estimated=result["tokens_after"],
                                  retained_fact="KEEP-7391" in result["messages"][0]["content"], status="ok")
                except Exception:
                    record["status"] = "failed"
                record["round_trip_ms"] = round((time.perf_counter() - started) * 1000, 2)
                records.append(record)
    print(json.dumps({"backend_requested": args.backend, "quality_claim": "single synthetic fact only; not a production quality evaluation",
                      "acceptance_thresholds": {"warm_round_trip_p95_ms_max": 200, "token_reduction_min_fraction": 0.2,
                                                "required_fact_retention": 1.0},
                      "production_approved": False,
                      "billing_savings_measured": False, "records": records}, indent=2))


if __name__ == "__main__":
    asyncio.run(main())
