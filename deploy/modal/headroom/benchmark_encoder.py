"""Bounded, offline CPU benchmark for the MiniLM ONNX encoder."""

import argparse
import json
import statistics
import time

DIMENSIONS = 384
SHORT_FIXTURE = "Find the failed transaction and its amount in the application logs."


def mean_pool_and_normalize(hidden, attention_mask):
    """Apply the production encoder's masked mean pooling and L2 normalization."""
    import numpy as np

    mask = attention_mask[..., None].astype(hidden.dtype, copy=False)
    pooled = (hidden * mask).sum(axis=1) / np.maximum(mask.sum(axis=1), 1)
    norms = np.linalg.norm(pooled, axis=1, keepdims=True)
    return pooled / np.maximum(norms, 1e-12)


class OnnxEncoder:
    def __init__(self, model_path, tokenizer_path, *, tokenizer=None, session=None):
        import numpy as np

        self.np = np
        started = time.perf_counter_ns()
        if tokenizer is None:
            from tokenizers import Tokenizer

            tokenizer = Tokenizer.from_file(str(tokenizer_path))
            tokenizer.no_truncation()
            tokenizer.no_padding()
        if session is None:
            import onnxruntime as ort

            options = ort.SessionOptions()
            options.intra_op_num_threads = 1
            options.inter_op_num_threads = 1
            options.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
            session = ort.InferenceSession(
                str(model_path), sess_options=options, providers=["CPUExecutionProvider"]
            )
        self.tokenizer = tokenizer
        self.session = session
        self.input_names = {item.name for item in session.get_inputs()}
        self.load_ms = (time.perf_counter_ns() - started) / 1_000_000

    def tokenize(self, texts):
        if not isinstance(texts, list) or not 1 <= len(texts) <= 32:
            raise ValueError("invalid embedding batch")
        if any(not isinstance(text, str) or not 1 <= len(text.encode()) <= 32768 for text in texts):
            raise ValueError("invalid embedding text")

        encodings = self.tokenizer.encode_batch(texts, add_special_tokens=True)
        lengths = [min(len(encoding.ids), 257) for encoding in encodings]
        attention_lengths = [sum(encoding.attention_mask[:length]) for encoding, length in zip(encodings, lengths)]
        if any(length > 256 for length in attention_lengths):
            raise ValueError("embedding input exceeds the model context")
        width = max(lengths)
        inputs = {
            "input_ids": self.np.zeros((len(texts), width), dtype=self.np.int64),
            "attention_mask": self.np.zeros((len(texts), width), dtype=self.np.int64),
        }
        token_types = self.np.zeros((len(texts), width), dtype=self.np.int64)
        for row, (encoding, length) in enumerate(zip(encodings, lengths)):
            inputs["input_ids"][row, :length] = encoding.ids[:length]
            inputs["attention_mask"][row, :length] = encoding.attention_mask[:length]
            token_types[row, :length] = encoding.type_ids[:length]
        if "token_type_ids" in self.input_names:
            inputs["token_type_ids"] = token_types
        return {name: value for name, value in inputs.items() if name in self.input_names}

    def infer(self, inputs):
        hidden = self.session.run(None, inputs)[0]
        vectors = mean_pool_and_normalize(hidden, inputs["attention_mask"])
        if vectors.shape != (inputs["input_ids"].shape[0], DIMENSIONS) or not self.np.isfinite(vectors).all():
            raise ValueError("invalid embedding output")
        return vectors

    def __call__(self, texts):
        return self.infer(self.tokenize(texts)).tolist()


def _near_context_fixture(encoder):
    words = ["transaction"] * 300
    low, high = 1, len(words)
    while low < high:
        middle = (low + high + 1) // 2
        try:
            encoder.tokenize([" ".join(words[:middle])])
            low = middle
        except ValueError:
            high = middle - 1
    text = " ".join(words[:low])
    if int(encoder.tokenize([text])["attention_mask"].sum()) < 240:
        raise RuntimeError("could not construct near-256-token fixture")
    return text


def _stats(values):
    return {
        "mean": statistics.fmean(values),
        "median": statistics.median(values),
        "min": min(values),
        "max": max(values),
    }


def run_benchmark(model_path, tokenizer_path, repetitions=30, warmups=3):
    encoder = OnnxEncoder(model_path, tokenizer_path)
    near = _near_context_fixture(encoder)
    batches = [[SHORT_FIXTURE], [near], [SHORT_FIXTURE, "error", near, "application log transaction amount"]]
    for index in range(warmups):
        encoder(batches[index % len(batches)])

    timings = [{"tokenization": [], "inference_and_pooling": [], "total": []} for _ in batches]
    first_vector = None
    for index in range(repetitions):
        batch = batches[index % len(batches)]
        start = time.perf_counter_ns()
        inputs = encoder.tokenize(batch)
        tokenized = time.perf_counter_ns()
        vectors = encoder.infer(inputs)
        finished = time.perf_counter_ns()
        group = timings[index % len(batches)]
        group["tokenization"].append((tokenized - start) / 1_000_000)
        group["inference_and_pooling"].append((finished - tokenized) / 1_000_000)
        group["total"].append((finished - start) / 1_000_000)
        if first_vector is None:
            first_vector = vectors[0].tolist()
    return {
        "load_ms": encoder.load_ms,
        "repetitions": repetitions,
        "warmups": warmups,
        "fixture_attention_lengths": [int(encoder.tokenize(batch)["attention_mask"].sum(axis=1).max()) for batch in batches],
        "timings_ms": {name: {span: _stats(values) for span, values in group.items() if values}
                       for name, group in zip(["short", "near_context", "mixed_batch"], timings)},
        "first_fixture_vector": first_vector,
    }


def parse_args(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True, help="local ONNX model path")
    parser.add_argument("--tokenizer", required=True, help="local tokenizer.json path")
    parser.add_argument("--repetitions", type=int, default=30, choices=range(1, 101), metavar="1..100")
    parser.add_argument("--warmups", type=int, default=3, choices=range(0, 11), metavar="0..10")
    return parser.parse_args(argv)


if __name__ == "__main__":
    args = parse_args()
    print(json.dumps(run_benchmark(args.model, args.tokenizer, args.repetitions, args.warmups), separators=(",", ":")))
