"""Pinned MiniLM mean-pooling embeddings for cache lookup and memory search."""
MODEL = "sentence-transformers/all-MiniLM-L6-v2"
DIMENSIONS = 384


class Encoder:
    def __init__(self, device):
        import torch
        from transformers import AutoModel, AutoTokenizer
        self.torch = torch
        self.device = device
        self.tokenizer = AutoTokenizer.from_pretrained(MODEL, local_files_only=True)
        self.model = AutoModel.from_pretrained(MODEL, local_files_only=True).to(device).eval()
        self(["readiness"])

    def __call__(self, texts):
        if not isinstance(texts, list) or not 1 <= len(texts) <= 32:
            raise ValueError("invalid embedding batch")
        if any(not isinstance(text, str) or not 1 <= len(text.encode()) <= 32768 for text in texts):
            raise ValueError("invalid embedding text")
        tokens = self.tokenizer(texts, padding=True, truncation=True, max_length=257, return_tensors="pt").to(self.device)
        # Never treat texts with identical prefixes and different omitted suffixes
        # as equivalent cache queries. Oversized inputs bypass semantic lookup.
        if (tokens["attention_mask"].sum(1) > 256).any():
            raise ValueError("embedding input exceeds the model context")
        with self.torch.inference_mode():
            hidden = self.model(**tokens).last_hidden_state
            mask = tokens["attention_mask"].unsqueeze(-1).to(hidden.dtype)
            pooled = (hidden * mask).sum(1) / mask.sum(1).clamp(min=1)
            vectors = self.torch.nn.functional.normalize(pooled, p=2, dim=1)
        if vectors.shape != (len(texts), DIMENSIONS) or not self.torch.isfinite(vectors).all():
            raise ValueError("invalid embedding output")
        return vectors.cpu().tolist()
