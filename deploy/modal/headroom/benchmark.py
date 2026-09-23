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
    args = parser.parse_args()
    if not 1 <= args.samples <= 20:
        parser.error("samples must be 1..20")
    url = os.environ.get("HEADROOM_BENCH_URL", "")
    if args.live and not url.startswith("https://"):
        parser.error("set HEADROOM_BENCH_URL to the authorized HTTPS origin")
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
                          "environment": "modal" if args.live else "orb-cpu", "process_state": "fresh-process"}
                try:
                    if args.live:
                        response = await client.post(url + "/v1/compress", json=body, headers={
                            "Modal-Key": os.environ["MODAL_TOKEN_ID"], "Modal-Secret": os.environ["MODAL_TOKEN_SECRET"],
                            "X-Headroom-Proxy-Token": os.environ["HEADROOM_PROXY_TOKEN"], "X-Headroom-Project": "a" * 64})
                        response.raise_for_status()
                        result = response.json()
                        record["service_ms"] = response.headers.get("x-compression-ms")
                    else:
                        result = json.loads(await isolated_compress(json.dumps(body).encode()))
                    record.update(tokens_before_estimated=result["tokens_before"], tokens_after_estimated=result["tokens_after"],
                                  retained_fact="KEEP-7391" in result["messages"][0]["content"], status="ok")
                except Exception:
                    record["status"] = "failed"
                record["round_trip_ms"] = round((time.perf_counter() - started) * 1000, 2)
                records.append(record)
    print(json.dumps({"gpu_enabled": False, "quality_claim": "single synthetic fact only",
                      "billing_savings_measured": False, "records": records}, indent=2))


if __name__ == "__main__":
    asyncio.run(main())
