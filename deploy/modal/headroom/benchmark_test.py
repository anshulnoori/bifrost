import unittest

import numpy as np

from benchmark import assess_result, cost_summary, parse_args
from audit_cost import audit
from benchmark_encoder import DIMENSIONS, OnnxEncoder, mean_pool_and_normalize


class _Encoding:
    def __init__(self, length):
        self.ids = list(range(length))
        self.attention_mask = [1] * length
        self.type_ids = [0] * length


class _Tokenizer:
    def encode_batch(self, texts, add_special_tokens=True):
        return [_Encoding(int(text)) for text in texts]


class _Input:
    def __init__(self, name):
        self.name = name


class _Session:
    def get_inputs(self):
        return [_Input("input_ids"), _Input("attention_mask"), _Input("token_type_ids")]

    def run(self, output_names, inputs):
        shape = (*inputs["input_ids"].shape, DIMENSIONS)
        return [np.ones(shape, dtype=np.float32)]


class EncoderBenchmarkTests(unittest.TestCase):
    def setUp(self):
        self.encoder = OnnxEncoder(None, None, tokenizer=_Tokenizer(), session=_Session())

    def test_context_boundary_accepts_256_and_rejects_257(self):
        self.assertEqual(self.encoder.tokenize(["256"])["input_ids"].shape, (1, 256))
        with self.assertRaisesRegex(ValueError, "model context"):
            self.encoder.tokenize(["257"])

    def test_pooling_masks_padding_and_normalizes(self):
        hidden = np.array([[[3.0, 0.0], [0.0, 4.0], [100.0, 100.0]]], dtype=np.float32)
        vectors = mean_pool_and_normalize(hidden, np.array([[1, 1, 0]], dtype=np.int64))
        np.testing.assert_allclose(vectors, [[0.6, 0.8]], rtol=1e-6)

    def test_input_bounds(self):
        for value in (None, [], ["1"] * 33):
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, "batch"):
                self.encoder(value)
        for value in ([1], [""], ["x" * 32769]):
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, "text"):
                self.encoder(value)

    def test_output_is_finite_and_384_dimensions(self):
        vectors = np.asarray(self.encoder(["3", "5"]))
        self.assertEqual(vectors.shape, (2, DIMENSIONS))
        self.assertTrue(np.isfinite(vectors).all())
        np.testing.assert_allclose(np.linalg.norm(vectors, axis=1), [1, 1], rtol=1e-6)


class BillingAuditTests(unittest.TestCase):
    def test_billing_uses_matching_app_and_bpe_not_headroom_words(self):
        records = [{"input_bytes": size, "tokens_before_estimated": words, "sample": sample,
                    "round_trip_ms": 200, "status": "ok"}
                   for size, words in [(1430, 160), (14490, 1600)] for sample in range(3)]
        billing = [{"object_id": "wanted", "cost": value} for value in ["0.00262208", "0.00044167", "0.00014221"]]
        billing.append({"object_id": "other", "cost": "999"})
        rates = {"gpu_hour_cost_t4": "0.59", "cpu_hour_cost": "0.04730", "mem_gib_hour_cost": "0.008"}
        result = audit({"records": records, "model_load_ms": 14000}, billing, rates, "wanted")
        self.assertEqual(result["reported_gross_cost_usd"], "0.00320596")
        self.assertEqual(result["successful_input_bpe_tokens"], 17832)
        self.assertAlmostEqual(result["gross_usd_per_million_input_bpe_tokens"], 0.17978689995513683)
        self.assertIsNone(result["cost_per_removed_bpe_token"])
        self.assertTrue(all(not group["eligible_at_current_16kib_gate"] for group in result["groups"]))
        self.assertAlmostEqual(result["requested_resources_usd_per_second"], 0.7166 / 3600)


class CostAccountingTests(unittest.TestCase):
    def test_all_failures_still_cost_but_have_no_cost_per_token(self):
        summary = cost_summary([{"status": "failed"}, {"status": "quality_failed"}], "t4", 10)

        self.assertAlmostEqual(summary["estimated_cost_usd"], 0.0019908)
        self.assertEqual(summary["successful_input_tokens"], 0)
        self.assertEqual(summary["successful_tokens_removed"], 0)
        self.assertIsNone(summary["cost_per_successful_input_token_usd"])
        self.assertIsNone(summary["cost_per_token_removed_usd"])
        self.assertEqual(summary["failed_or_quality_failed_samples"], 2)
        self.assertEqual(summary["count_unit"], "headroom_whitespace_words_not_provider_bpe_tokens")
        self.assertFalse(summary["provider_token_cost_known"])

    def test_uses_actual_unequal_token_counts_not_sample_averages(self):
        records = [
            {"status": "ok", "tokens_before_estimated": 100, "tokens_after_estimated": 60},
            {"status": "ok", "tokens_before_estimated": 900, "tokens_after_estimated": 800},
            {"status": "failed", "tokens_before_estimated": 10000, "tokens_after_estimated": 0},
        ]

        summary = cost_summary(records, "l4", 2)

        self.assertEqual(summary["successful_input_tokens"], 1000)
        self.assertEqual(summary["successful_tokens_removed"], 140)
        self.assertAlmostEqual(summary["cost_per_successful_input_token_usd"], 0.00051416 / 1000)
        self.assertAlmostEqual(summary["cost_per_token_removed_usd"], 0.00051416 / 140)

    def test_zero_savings_is_quality_failure_and_gets_no_credit(self):
        assessed = assess_result({
            "tokens_before": 40, "tokens_after": 40,
            "messages": [{"content": "KEEP-7391"}],
        })

        self.assertEqual(assessed["status"], "quality_failed")
        summary = cost_summary([assessed], "cpu", 5)
        self.assertGreater(summary["estimated_cost_usd"], 0)
        self.assertIsNone(summary["cost_per_token_removed_usd"])

    def test_missing_sentinel_is_quality_failure(self):
        assessed = assess_result({
            "tokens_before": 40, "tokens_after": 20,
            "messages": [{"content": "sentinel removed"}],
        })

        self.assertEqual(assessed["status"], "quality_failed")


class ArgumentTests(unittest.TestCase):
    def test_live_requires_explicit_billing_basis(self):
        with self.assertRaises(SystemExit):
            parse_args(["--live", "--backend", "t4"])

    def test_sample_count_remains_strictly_bounded(self):
        for count in (0, 21):
            with self.subTest(count=count), self.assertRaises(SystemExit):
                parse_args(["--samples", str(count)])

    def test_local_gpu_is_rejected(self):
        with self.assertRaises(SystemExit):
            parse_args(["--backend", "l4"])


if __name__ == "__main__":
    unittest.main()
