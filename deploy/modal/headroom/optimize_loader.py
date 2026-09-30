"""Build-time patch for the pinned Headroom loader; never change scoring."""
import hashlib
import importlib.util
from pathlib import Path

SOURCE_SHA256 = "de164862246c8251417342f46d562f3bf64e09d01a50c86b62be74006b025762"


def optimize(source):
    if hashlib.sha256(source).hexdigest() != SOURCE_SHA256:
        raise ValueError("Headroom loader changed; review optimization before upgrading")
    text = source.decode()
    old = 'self.encoder = AutoModel.from_pretrained(model_name, attn_implementation="eager")'
    new = ('from transformers import AutoConfig\n'
           '            self.encoder = AutoModel.from_config(\n'
           '                AutoConfig.from_pretrained(model_name, local_files_only=True),\n'
           '                attn_implementation="eager")')
    if text.count(old) != 1:
        raise ValueError("unexpected model constructor")
    # The pinned loader rejects missing/unexpected checkpoint keys before use.
    result = text.replace(old, new)
    compile(result, "kompress_compressor.py", "exec")
    return result.encode()


if __name__ == "__main__":
    spec = importlib.util.find_spec("headroom.transforms.kompress_compressor")
    path = Path(spec.origin)
    path.write_bytes(optimize(path.read_bytes()))
