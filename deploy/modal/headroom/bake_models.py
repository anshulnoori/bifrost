"""Pinned public model downloads into a versioned Volume, before deployment."""
from pathlib import Path
from huggingface_hub import snapshot_download

CACHE = Path("/opt/headroom-models")
MODELS = {
    "chopratejas/kompress-v2-base": (
        "b1563631b35bfdcee37587ad530147497d820d4c", ["merged.pt", "onnx/kompress-int8-wo.onnx"]),
    "answerdotai/ModernBERT-base": (
        "8949b909ec900327062f0ebf497f51aef5e6f0c8",
        ["config.json", "tokenizer.json", "tokenizer_config.json", "special_tokens_map.json"]),
    "sentence-transformers/all-MiniLM-L6-v2": (
        "1110a243fdf4706b3f48f1d95db1a4f5529b4d41",
        ["config.json", "model.safetensors", "tokenizer.json", "tokenizer_config.json", "special_tokens_map.json", "vocab.txt"]),
}


def bake():
    for model, (revision, files) in MODELS.items():
        snapshot_download(model, revision=revision, allow_patterns=files, cache_dir=CACHE, token=False)
        # Upstream asks for main. Pin that local ref to the downloaded commit;
        # offline runtime prevents it from ever following a moving remote main.
        ref = CACHE / ("models--" + model.replace("/", "--")) / "refs" / "main"
        ref.parent.mkdir(parents=True, exist_ok=True)
        ref.write_text(revision)


if __name__ == "__main__":
    bake()
