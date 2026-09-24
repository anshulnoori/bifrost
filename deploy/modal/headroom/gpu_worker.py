"""Private framed subprocess. Reuse CUDA weights, never response or CCR state."""
import contextlib
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
    if compressor.preload(allow_download=False) != "pytorch":
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
        if isinstance(body, dict) and set(body) == {"operation", "input"} and body["operation"] == "embed" and encoder is not None:
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
