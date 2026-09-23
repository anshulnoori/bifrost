# Measurements and cost model

The orb CPU benchmark uses official Headroom 0.38.0 in a fresh isolated subprocess for
every request. Two samples per size, no network hop, one synthetic repeated JSON fixture:

| Input bytes | Estimated tokens before → after | Round-trip range |
| --- | --- | --- |
| 854 | 336 → 336 | 5.13–5.23 seconds |
| 14,782 | 5,520 → 2,269 | 5.05–5.40 seconds |
| 120,102 | 44,876 → 18,682 | 5.20–5.25 seconds |

All samples retained one sentinel fact. That is not evidence of general fact retention or
no quality loss. The results are not Modal measurements. A concurrent-build run exceeded
the 25-second compressor timeout. The real gateway fixture passed when rerun without build
contention. The default two-second bridge deadline therefore cannot use this CPU profile
reliably. Production compression remains disabled; do not silently increase the deadline.

Run `python deploy/modal/headroom/benchmark.py --samples 3` for local synthetic results.
Only after spend approval, use `--live` with the Modal origin and secrets injected from a
private environment. The script emits counts/times, not prompts. It does not yet distinguish
platform cold starts from warm starts or attribute billing cost.

Before enabling compression, compare bypass, CPU, and approved GPU variants with 1KiB,
16KiB, 128KiB, and realistic private tool outputs. Collect cold/warm RTT, service time,
concurrency1/4/16, token estimates, actual provider usage, quality fixtures, and billed
compute seconds. Require zero protected-field changes, documented fact-retention acceptance,
and p95 overhead below the owner's latency budget. Select a byte threshold only where
measured net benefit remains positive. No GPU threshold or GPU deployment is enabled here.
The selected structural compressor has ML disabled; adding a GPU alone may do nothing.

## Illustrative monthly infrastructure costs

These are planning estimates, not measured invoices. Recheck pricing before provisioning.
Cloudflare standard-1 provides 0.5 vCPU, 4GiB memory, and 8GB disk. At listed rates of
$0.000020 per CPU-second, $0.0000025 per GiB-second, and $0.00000007 per GB-second,
one fully busy allocated hour is approximately $0.074, before included usage and egress.
CPU is usage-metered; memory/disk residency and five-minute idle tails matter.
Workers paid starts at $5/month. Included Container buckets can reduce the marginal cost.

| Scenario | Assumptions | Conservative Container + Workers estimate |
| --- | --- | --- |
| Bursty personal | 30 allocated hours/month | about $7.22 before allowances/egress |
| Moderate multi-agent | 240 allocated hours/month | about $22.76 before allowances/egress |

At an illustrative Neon compute rate of $0.106/CU-hour, 0.25CU for those same active hours
adds about $0.80 or $6.36. Ten GB at $0.35/GB-month adds $3.50. Minimum plan charges,
history, backup, and transfer can add costs. These scenarios do not establish actual Neon
wake residency or Cloudflare CPU utilization.

Modal cost equals measured CPU/GPU and memory seconds times current rates, including cold
starts and idle tails. No live data exists to price it credibly; disabled compression incurs
no request compute. Add inference-provider tokens separately. Do not subtract estimated
compression savings from a bill. Access, Tailscale, DNS, and applicable plan charges are extra.

Sources: https://developers.cloudflare.com/containers/pricing/,
https://neon.com/pricing, https://modal.com/pricing.
