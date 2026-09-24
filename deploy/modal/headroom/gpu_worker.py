"""Private framed subprocess. Reuse CUDA weights, never response or CCR state."""
import contextlib
import json
import logging
import struct
import sys

from service import MAX_BODY, validate


def load_compressor():
    import torch
    from headroom.transforms.kompress_compressor import KompressCompressor, KompressConfig

    if not torch.cuda.is_available():
        raise RuntimeError("CUDA unavailable")
    compressor = KompressCompressor(KompressConfig(device="cuda", enable_ccr=False))
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


def serve(reader, writer, compressor):
    writer.write(b"CUDA\n")
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
        output = compress(raw, compressor)
        if len(output) > MAX_BODY:
            raise ValueError()
        writer.write(struct.pack("!I", len(output)) + output)
        writer.flush()
        # Drop request/response references before waiting for another request.
        raw = output = None


if __name__ == "__main__":
    logging.disable(logging.CRITICAL)
    output = sys.stdout.buffer
    try:
        with contextlib.redirect_stdout(sys.stderr):
            serve(sys.stdin.buffer, output, load_compressor())
    except Exception:
        # Prompt-bearing exceptions must never reach platform logs.
        sys.exit(1)
