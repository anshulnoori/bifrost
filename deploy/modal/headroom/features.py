"""Durable CCR and official Headroom memory tools behind the authenticated facade."""
import asyncio
import hashlib
import json
import logging
import math
import re
import secrets
from datetime import datetime

from state import StateError

CCR_TTL = 1800
MEMORY_TTL = 7 * 86400
TOOLS = {"headroom_retrieve", "memory_save", "memory_search", "memory_update", "memory_delete"}


async def state_call(function, *args):
    # A DB thread cannot be cancelled by asyncio. Keep the service slot until
    # its bounded transaction finishes, including when the HTTP client leaves.
    task = asyncio.create_task(asyncio.to_thread(function, *args))
    try:
        return await asyncio.shield(task)
    except asyncio.CancelledError:
        await asyncio.gather(task, return_exceptions=True)
        raise


class StateFeatures:
    def __init__(self, store, embed):
        self.store, self.embed = store, embed

    def retain(self, scope, originals, compressed):
        """Publish handles only after the entire transaction commits."""
        result, handles = [], []
        with self.store.transaction(scope) as state:
            for original, text in zip(originals, compressed, strict=True):
                handle = secrets.token_hex(32)
                marker = f"\n<<ccr:{handle}>> Use headroom_retrieve to recover omitted details."
                if len((text + marker).encode()) >= len(original.encode()):
                    result.append(text)
                    handles.append("")
                    continue
                state.put("ccr", handle, {"original": original, "compressed": text}, CCR_TTL)
                result.append(text + marker)
                handles.append(handle)
        return result, handles

    async def tool(self, scope, name, arguments, operation_id):
        if name not in TOOLS or not isinstance(arguments, dict):
            raise ValueError("invalid internal tool")
        if not isinstance(operation_id, str) or not re.fullmatch(r"[a-f0-9]{64}", operation_id):
            raise ValueError("invalid operation identifier")
        # Narrow the official tool API: model arguments cannot supply identity,
        # arbitrary metadata, background work, external endpoints or graph writes.
        fields = {
            "headroom_retrieve": ({"hash"}, {"hash"}),
            "memory_save": ({"content", "importance"}, {"content", "importance"}),
            "memory_search": ({"query", "top_k"}, {"query"}),
            "memory_update": ({"memory_id", "new_content", "reason"}, {"memory_id", "new_content", "reason"}),
            "memory_delete": ({"memory_id", "reason"}, {"memory_id", "reason"}),
        }
        allowed, required = fields[name]
        if set(arguments) - allowed or not required <= set(arguments):
            raise ValueError("invalid internal tool arguments")
        for key in {"hash", "memory_id"} & set(arguments):
            if not isinstance(arguments[key], str) or not re.fullmatch(r"[a-f0-9]{64}", arguments[key]):
                raise ValueError("invalid state identifier")
        for key in {"content", "new_content", "query", "reason"} & set(arguments):
            if not isinstance(arguments[key], str) or not 1 <= len(arguments[key].encode()) <= 8192:
                raise ValueError("invalid memory text")
        if name == "memory_save" and (type(arguments["importance"]) not in {float, int} or not 0 <= arguments["importance"] <= 1):
            raise ValueError("invalid memory importance")
        if "top_k" in arguments and (type(arguments["top_k"]) is not int or not 1 <= arguments["top_k"] <= 10):
            raise ValueError("invalid result count")
        text = arguments.get("content", arguments.get("new_content", arguments.get("query")))
        vector = (await self.embed([text]))[0] if text else None
        if vector is not None and (len(vector) != 384 or any(type(x) not in {int, float} or not math.isfinite(x) for x in vector) or abs(sum(x*x for x in vector) - 1) > 0.01):
            raise ValueError("invalid memory embedding")
        # Do not keep a DB transaction open during model inference.
        return await state_call(self._tool, scope, name, arguments, operation_id, vector)

    def _tool(self, scope, name, arguments, operation_id, vector):
        from headroom.memory.system import MemorySystem
        # Upstream's INFO and exception logs include memory contents.
        logging.getLogger("headroom.memory.system").disabled = True
        digest = hashlib.sha256(json.dumps([name, arguments], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        with self.store.transaction(scope) as state:
            # Idempotency is bound to the authenticated scope and original call ID.
            previous = state.get("operation", operation_id)
            if previous:
                if previous["digest"] != digest:
                    raise StateError("state operation failed")
                return previous["result"]
            if name == "headroom_retrieve":
                entry = state.get("ccr", arguments["hash"])
                if entry is None:
                    raise StateError("state operation failed")
                return {"success": True, "content": entry["original"]}
            backend = MemoryBackend(state, operation_id, vector)
            system = MemorySystem(backend, user_id=scope, session_id=scope)
            result = asyncio.run(system.process_tool_call(name, arguments))
            if not result.get("success"):
                # Do not expose upstream exceptions or partial mutation results.
                raise StateError("state operation failed")
            if name != "memory_search":
                # Replaying a mutation must not recover text after its deletion.
                result = {"success": True, "memory_id": result.get("memory_id", arguments.get("memory_id"))}
                state.put("operation", operation_id, {"digest": digest, "result": result}, MEMORY_TTL)
            return result


class MemoryBackend:
    """Headroom MemoryBackend with an already-authorized encrypted transaction."""
    supports_graph = False
    supports_vector_search = True

    def __init__(self, state, operation_id, vector):
        self.state, self.operation_id, self.vector = state, operation_id, vector

    async def save_memory(self, content, user_id, importance, session_id=None, **_):
        import numpy as np
        from headroom.memory.models import Memory
        memory = Memory(id=self.operation_id, content=content, user_id=self.state.scope,
                        session_id=self.state.scope, importance=importance,
                        embedding=np.array(self.vector, dtype=np.float32))
        self.state.put("memory", memory.id, memory.to_dict(), MEMORY_TTL)
        return memory

    async def get_memory(self, memory_id):
        from headroom.memory.models import Memory
        value = self.state.get("memory", memory_id)
        return Memory.from_dict(value) if value else None

    async def search_memories(self, query, user_id, top_k=10, **_):
        from headroom.memory.models import Memory
        from headroom.memory.ports import MemorySearchResult
        results = []
        for _, value in self.state.list("memory"):
            if value is None or value.get("valid_until"):
                continue
            vector = value.get("embedding") or []
            if len(vector) != len(self.vector):
                raise StateError("state operation failed")
            score = sum(a * b for a, b in zip(vector, self.vector))
            if math.isfinite(score) and score >= 0.25:
                results.append(MemorySearchResult(Memory.from_dict(value), score))
        return sorted(results, key=lambda item: (-item.score, item.memory.id))[:top_k]

    async def update_memory(self, memory_id, new_content, reason=None, user_id=None):
        old = await self.get_memory(memory_id)
        if old is None or not old.is_current:
            raise StateError("state operation failed")
        new = await self.save_memory(new_content, self.state.scope, old.importance)
        old.valid_until = datetime.utcnow()
        old.superseded_by, new.supersedes = new.id, old.id
        self.state.put("memory", old.id, old.to_dict(), MEMORY_TTL)
        self.state.put("memory", new.id, new.to_dict(), MEMORY_TTL)
        return new

    async def delete_memory(self, memory_id, **_):
        target = await self.get_memory(memory_id)
        if target is None:
            return False
        # Erase the whole version chain, not just the latest searchable version.
        identifiers = {memory_id}
        memories = self.state.list("memory")
        changed = True
        while changed:
            changed = False
            for identifier, value in memories:
                if value and identifier not in identifiers and (value.get("supersedes") in identifiers or value.get("superseded_by") in identifiers):
                    identifiers.add(identifier)
                    changed = True
        for identifier in identifiers:
            self.state.delete("memory", identifier)
        return True

    async def close(self):
        pass
