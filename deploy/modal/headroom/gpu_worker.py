"""Private framed subprocess. Reuse CUDA weights, never response or CCR state."""
import contextlib
import gc
import json
import logging
import os
import struct
import sys

from service import MAX_BODY, validate


def load_compressor(device="cuda"):
    import torch
    from headroom.transforms.kompress_compressor import KompressCompressor, KompressConfig

    if device not in {"cuda", "cpu"} or (device == "cuda" and not torch.cuda.is_available()):
        raise RuntimeError("CUDA unavailable")
    compressor = KompressCompressor(KompressConfig(device=device, enable_ccr=False))
    attention = os.environ.get("HEADROOM_ATTENTION", "eager")
    precision = os.environ.get("HEADROOM_PRECISION", "default")
    if attention not in {"eager", "sdpa"} or precision not in {"default", "float16", "bfloat16"}:
        raise ValueError("invalid inference configuration")
    if device == "cuda" and (attention != "eager" or precision != "default"):
        # Configure the pinned cached model before preload starts its canary.
        # Casting the whole scorer includes both heads, not only the encoder.
        from headroom.transforms.kompress_compressor import _load_kompress
        model, _, backend = _load_kompress(compressor.config.model_id, device, allow_download=False)
        if backend != "pytorch":
            raise RuntimeError("unexpected compression backend")
        model.encoder.set_attn_implementation(attention)
        if precision != "default":
            model.to(dtype=getattr(torch, precision))
    if compressor.preload(allow_download=False) != ("onnx" if device == "cpu" else "pytorch"):
        raise RuntimeError("unexpected compression backend")
    return compressor


def compress(raw, compressor):
    body = json.loads(raw)
    validate(body)
    messages, before, after = [], 0, 0
    for message in body["messages"]:
        result = compressor.compress(message["content"], allow_download=False)
        messages.append({**message, "content": result.compressed})
        before += result.original_tokens
        after += result.compressed_tokens
    return json.dumps({"messages": messages, "tokens_before": before,
                       "tokens_after": after, "ccr_hashes": [], "obligations": []}).encode()


def prepare_snapshot(compressor):
    import torch

    # preload() starts an optional canary thread in the pinned Headroom release.
    canary = getattr(compressor, "_canary_thread", None)
    if canary is not None:
        canary.join(timeout=5)
        if canary.is_alive():
            raise RuntimeError("startup probe still running")
    torch.cuda.synchronize()
    before = torch.cuda.memory_reserved()
    gc.collect()
    torch.cuda.empty_cache()
    torch.cuda.synchronize()
    # Keep model weights alive; only unused allocation blocks are released.
    return {"cuda_reserved_before": before,
            "cuda_reserved_after": torch.cuda.memory_reserved(),
            "cuda_allocated": torch.cuda.memory_allocated()}


def serve(reader, writer, compressor, encoder=None, device="cuda"):
    writer.write(f"{device.upper():4}\n".encode())
    writer.flush()
    while header := reader.read(4):
        if len(header) != 4:
            raise ValueError()
        size = struct.unpack("!I", header)[0]
        if not 0 < size <= MAX_BODY:
            raise ValueError()
        raw = reader.read(size)
        if len(raw) != size:
            raise ValueError()
        body = json.loads(raw)
        if body == {"operation": "snapshot"} and device == "cuda":
            output = json.dumps(prepare_snapshot(compressor)).encode()
        elif isinstance(body, dict) and set(body) == {"operation", "input"} and body["operation"] == "embed" and encoder is not None:
            output = json.dumps({"vectors": encoder(body["input"])}).encode()
        else:
            output = compress(raw, compressor)
        if len(output) > MAX_BODY:
            raise ValueError()
        writer.write(struct.pack("!I", len(output)) + output)
        writer.flush()
        # Drop request/response references before waiting for another request.
        raw = output = body = None


if __name__ == "__main__":
    logging.disable(logging.CRITICAL)
    output = sys.stdout.buffer
    try:
        with contextlib.redirect_stdout(sys.stderr):
            from encoder import Encoder
            device = os.environ.get("HEADROOM_WORKER_DEVICE", "cuda")
            serve(sys.stdin.buffer, output, load_compressor(device), Encoder(device), device)
    except Exception:
        # Prompt-bearing exceptions must never reach platform logs.
        sys.exit(1)
