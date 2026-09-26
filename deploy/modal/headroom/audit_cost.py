"""Offline audit of validate_gpu.py fixtures against Modal billing exports."""
import argparse
from decimal import Decimal
import json
from pathlib import Path
import statistics


def audit(report, billing, rates, app_id):
    import tiktoken
    encoding = tiktoken.get_encoding("o200k_base")
    fixtures = {}
    for rows in (20, 200):
        text = json.dumps([{"id": i, "region": "east", "fact": "KEEP-7391", "status": "healthy"} for i in range(rows)])
        fixtures[len(text.encode())] = (len(text.split()), len(encoding.encode(text)))
    resources = [r for r in billing if r["object_id"] == app_id]
    if not resources:
        raise ValueError("no billing rows for validation app")
    cost = sum((Decimal(r["cost"]) for r in resources), Decimal(0))
    hourly = sum(Decimal(rates[k]) * n for k, n in [
        ("gpu_hour_cost_t4", 1), ("cpu_hour_cost", 2), ("mem_gib_hour_cost", 4)])
    per_second = hourly / 3600
    total_tokens = 0
    groups = []
    for size, (words, bpe) in fixtures.items():
        records = [r for r in report["records"] if r["input_bytes"] == size]
        if not records or any(r["tokens_before_estimated"] != words for r in records):
            raise ValueError("report does not match the known validation fixture")
        total_tokens += sum(bpe for r in records if r["status"] == "ok")
        warm = [r["round_trip_ms"] for r in records if r["sample"] > 0 and r["status"] == "ok"]
        seconds = statistics.median(warm) / 1000
        groups.append({"input_bytes": size, "headroom_input_words": words, "input_bpe_tokens": bpe,
                       "warm_seconds": seconds,
                       "warm_usd_per_million_input_bpe_tokens": float(per_second * Decimal(str(seconds)) * 1000000 / bpe),
                       "eligible_at_current_16kib_gate": size >= 16384})
    if len(report["records"]) != sum(sum(r["input_bytes"] == size for r in report["records"]) for size in fixtures):
        raise ValueError("unknown fixture")
    return {"billing_app_id": app_id, "reported_gross_cost_usd": str(cost), "billing_rows": resources,
            "billing_note": "provider report before credits, subject to collection delays",
            "token_encoding": encoding.name, "successful_input_bpe_tokens": total_tokens,
            "gross_usd_per_million_input_bpe_tokens": float(cost * 1000000 / total_tokens) if total_tokens else None,
            "cost_per_removed_bpe_token": None,
            "removed_token_limitation": "compressed text was not retained; Headroom counts are whitespace words, not BPE tokens",
            "requested_resources_usd_per_second": float(per_second),
            "resource_limitation": "CPU/memory billed at max(requested, actual); warm estimates assume requested quantities",
            "model_load_seconds": report["model_load_ms"] / 1000,
            "model_load_plus_30s_idle_estimate_usd": float(per_second * (Decimal(str(report["model_load_ms"])) / 1000 + 30)),
            "groups": groups,
            "monthly_estimate_requirements": "eligible tool-input BPE tokens actually compressed, matching tokenizer, active/idle intervals and cold starts; not total LLM tokens"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    for name in ("report", "billing", "rates", "app-id"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    result = audit(*(json.loads(Path(p).read_text()) for p in (args.report, args.billing, args.rates)), args.app_id)
    print(json.dumps(result, indent=2))
