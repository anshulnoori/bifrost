"""Bounded synthetic compression benchmark. Live calls require explicit --live."""
import argparse
import asyncio
import json
import math
import os
import time

PRICING_SOURCE = "https://modal.com/pricing"
PRICING_AS_OF = "2026-09-26"
RATES_PER_SECOND = {
    # Modal's physical-core and GiB rates; this benchmark's CPU shape is 2 cores/4 GiB.
    "cpu": 2 * 0.0000131 + 4 * 0.00000222,
    "t4": 0.000164 + 2 * 0.0000131 + 4 * 0.00000222,
    "l4": 0.000222 + 2 * 0.0000131 + 4 * 0.00000222,
}
EXPECTED_POLICIES = {
    "cpu": "onnx-cpu-kompress-v2",
    "t4": "cuda-kompress-marker-free-v1",
    "l4": "cuda-kompress-marker-free-v1",
}
SIZES = (1024, 16384, 131072)


def assess_result(result):
    """Return quality status and token counts without crediting invalid output."""
    before = result.get("tokens_before")
    after = result.get("tokens_after")
    messages = result.get("messages")
    retained = (
        isinstance(messages, list)
        and bool(messages)
        and isinstance(messages[0], dict)
        and "KEEP-7391" in messages[0].get("content", "")
    )
    positive_reduction = (
        type(before) is int and type(after) is int and before > 0 and 0 <= after < before
    )
    return {
        "tokens_before_estimated": before,
        "tokens_after_estimated": after,
        "retained_fact": retained,
        "status": "ok" if retained and positive_reduction else "quality_failed",
    }


def cost_summary(records, backend, billable_seconds):
    """Charge the full measured interval, including unsuccessful attempts."""
    successful = [record for record in records if record.get("status") == "ok"]
    input_tokens = sum(record["tokens_before_estimated"] for record in successful)
    removed_tokens = sum(
        record["tokens_before_estimated"] - record["tokens_after_estimated"]
        for record in successful
    )
    estimated_cost = billable_seconds * RATES_PER_SECOND[backend]
    return {
        "count_unit": "headroom_whitespace_words_not_provider_bpe_tokens",
        "provider_token_cost_known": False,
        "billable_seconds": billable_seconds,
        "estimated_cost_usd": estimated_cost,
        "successful_input_tokens": input_tokens,
        "successful_tokens_removed": removed_tokens,
        "cost_per_successful_input_token_usd": (
            estimated_cost / input_tokens if input_tokens else None
        ),
        "cost_per_token_removed_usd": estimated_cost / removed_tokens if removed_tokens else None,
        "failed_or_quality_failed_samples": len(records) - len(successful),
    }


def parse_args(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--samples", type=int, default=3)
    parser.add_argument("--backend", choices=["cpu", "t4", "l4"], default="cpu")
    billing = parser.add_mutually_exclusive_group()
    billing.add_argument("--billable-seconds", type=float)
    billing.add_argument("--billable-overhead-seconds", type=float)
    args = parser.parse_args(argv)
    if not 1 <= args.samples <= 20:
        parser.error("samples must be 1..20")
    if args.billable_seconds is not None and (not math.isfinite(args.billable_seconds) or args.billable_seconds <= 0):
        parser.error("billable-seconds must be positive")
    if args.billable_overhead_seconds is not None and (not math.isfinite(args.billable_overhead_seconds) or args.billable_overhead_seconds <= 0):
        parser.error("billable-overhead-seconds must be positive")
    if args.live and args.billable_seconds is None and args.billable_overhead_seconds is None:
        parser.error("live cost comparison requires measured --billable-seconds or explicit --billable-overhead-seconds")
    if not args.live and args.backend != "cpu":
        parser.error("T4/L4 measurement requires the explicitly authorized live endpoint")
    return args


async def run(args):
    import httpx

    url = os.environ.get("HEADROOM_BENCH_URL", "")
    if args.live and not url.startswith("https://"):
        raise ValueError("set HEADROOM_BENCH_URL to the authorized HTTPS origin")

    records = []
    measurement_started = time.perf_counter()
    async with httpx.AsyncClient(timeout=60, follow_redirects=False, trust_env=False) as client:
        for size in SIZES:
            content = json.dumps([
                {"id": i, "region": "east", "fact": "KEEP-7391", "status": "healthy"}
                for i in range(max(1, size // 80))
            ])
            body = {
                "model": "gpt-4.1",
                "messages": [{"role": "tool", "tool_call_id": "slot-0", "content": content}],
                "config": {"protect_recent": 0, "compress_user_messages": False},
                "gateway": {"can_redrive": False, "can_relay_response": False,
                            "session_affinity": False, "plugin_version": "bifrost-headroom/1"},
            }
            for sample in range(args.samples):
                started = time.perf_counter()
                record = {
                    "target_bytes": size, "input_bytes": len(content.encode()), "sample": sample,
                    "environment": "modal-live" if args.live else "local-isolated-diagnostic",
                    "backend_requested": args.backend,
                    "process_state": "unknown-modal-placement" if args.live else "fresh-process",
                }
                try:
                    if args.live:
                        response = await client.post(url + "/v1/compress", json=body, headers={
                            "Modal-Key": os.environ["HEADROOM_MODAL_KEY"],
                            "Modal-Secret": os.environ["HEADROOM_MODAL_SECRET"],
                            "X-Headroom-Proxy-Token": os.environ["HEADROOM_PROXY_TOKEN"],
                            "X-Headroom-Project": "a" * 64,
                        })
                        response.raise_for_status()
                        result = response.json()
                        record["service_ms"] = response.headers.get("x-compression-ms")
                        record["policy"] = response.headers.get("x-headroom-policy")
                        if record["policy"] != EXPECTED_POLICIES[args.backend]:
                            raise ValueError("unexpected backend policy")
                    else:
                        from service import isolated_compress

                        result = json.loads(await isolated_compress(json.dumps(body).encode()))
                        record["policy"] = "local-isolated-compressor-not-onnx"
                    record.update(assess_result(result))
                except Exception:
                    record["status"] = "failed"
                record["round_trip_ms"] = round((time.perf_counter() - started) * 1000, 2)
                records.append(record)
    measured_window = time.perf_counter() - measurement_started

    report = {
        "backend_requested": args.backend,
        "expected_policy": EXPECTED_POLICIES[args.backend] if args.live else None,
        "quality_claim": "single synthetic fact only; not a production quality evaluation",
        "production_approved": False,
        "billing_savings_measured": False,
        "hardware_validation_limitation": (
            "The policy validates the software path, not the Modal GPU model; verify T4/L4 placement separately."
            if args.live and args.backend in {"t4", "l4"} else None
        ),
        "records": records,
    }
    if args.live:
        if args.billable_seconds is not None:
            billable_seconds = args.billable_seconds
            basis = "explicit measured billable-seconds override"
        else:
            billable_seconds = measured_window + args.billable_overhead_seconds
            basis = "single non-overlapping benchmark window plus explicit startup/scaledown overhead"
        report["cost_estimate"] = {
            **cost_summary(records, args.backend, billable_seconds),
            "rate_usd_per_second": RATES_PER_SECOND[args.backend],
            "pricing_source": PRICING_SOURCE,
            "pricing_as_of": PRICING_AS_OF,
            "billing_basis": basis,
            "disclaimer": "Estimate only, not a Modal invoice. Round-trip samples are not summed as billable time.",
        }
    else:
        report["cost_estimate"] = None
        report["local_limitation"] = "Local isolated compression is diagnostic only and is not ONNX CPU."
    return report


async def main():
    args = parse_args()
    try:
        report = await run(args)
    except ValueError as exc:
        raise SystemExit(str(exc)) from None
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    asyncio.run(main())
