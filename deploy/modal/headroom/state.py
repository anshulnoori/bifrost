"""Bounded, encrypted, tenant-scoped state. Schema changes never run at startup."""
import hashlib
import json
import os
import re
import time
from contextlib import contextmanager

from cryptography.hazmat.primitives.ciphers.aead import AESGCM
import psycopg
from psycopg.conninfo import conninfo_to_dict

MAX_VALUE = 4 * 1024 * 1024
MAX_SCOPE_BYTES = 32 * 1024 * 1024
MAX_SCOPE_ENTRIES = 256
KINDS = {"ccr", "memory", "learning", "operation"}


class StateError(Exception):
    """Only this generic error may cross the service boundary."""


class StateStore:
    def __init__(self, dsn, key, *, connect=psycopg.connect):
        if len(key) != 32:
            raise ValueError("state encryption requires a 32-byte key")
        self.dsn = dsn
        self.cipher = AESGCM(key)
        self.connect = connect

    @classmethod
    def from_environment(cls):
        dsn = os.environ.get("HEADROOM_DATABASE_URL", "")
        try:
            info = conninfo_to_dict(dsn)
            key = bytes.fromhex(os.environ.get("HEADROOM_STATE_KEY", ""))
        except (ValueError, psycopg.Error):
            raise ValueError("invalid Headroom state configuration") from None
        host = info.get("host", "")
        if not re.fullmatch(r"[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)*\.neon\.tech", host) or info.get("sslmode") != "verify-full" or info.get("hostaddr"):
            raise ValueError("Headroom state requires a verified-TLS Neon connection")
        return cls(dsn, key)

    @contextmanager
    def transaction(self, scope):
        if not isinstance(scope, str) or not re.fullmatch(r"[a-f0-9]{64}", scope):
            raise StateError("state operation failed")
        try:
            with self.connect(self.dsn, connect_timeout=5) as connection:
                with connection.cursor() as cursor:
                    cursor.execute("SET LOCAL statement_timeout = '5s'")
                    cursor.execute("SET LOCAL lock_timeout = '2s'")
                    # A transaction lock also works through Neon's pooled endpoint.
                    lock = int.from_bytes(hashlib.sha256(scope.encode()).digest()[:8], "big", signed=True)
                    cursor.execute("SELECT pg_advisory_xact_lock(%s)", (lock,))
                    cursor.execute("DELETE FROM headroom.entries WHERE scope = %s AND expires_at <= now()", (scope,))
                    yield ScopedState(cursor, self.cipher, scope)
        except StateError:
            raise
        except Exception:
            # Driver errors can contain connection strings, values, or ciphertext.
            raise StateError("state operation failed") from None


class ScopedState:
    def __init__(self, cursor, cipher, scope):
        self.cursor, self.cipher, self.scope = cursor, cipher, scope

    def aad(self, kind, identifier):
        if kind not in KINDS or not isinstance(identifier, str) or not re.fullmatch(r"[a-f0-9]{1,64}", identifier):
            raise StateError("state operation failed")
        return json.dumps([self.scope, kind, identifier], separators=(",", ":")).encode()

    def get(self, kind, identifier):
        aad = self.aad(kind, identifier)
        self.cursor.execute("SELECT ciphertext FROM headroom.entries WHERE scope = %s AND kind = %s AND id = %s AND expires_at > now()",
                            (self.scope, kind, identifier))
        row = self.cursor.fetchone()
        if row is None:
            return None
        raw = bytes(row[0])
        envelope = json.loads(self.cipher.decrypt(raw[:12], raw[12:], aad))
        # Expiry is authenticated too: changing the indexed DB timestamp cannot
        # extend a capability's lifetime.
        return envelope["value"] if envelope["expires_at"] > time.time() else None

    def put(self, kind, identifier, value, ttl):
        aad = self.aad(kind, identifier)
        if type(ttl) is not int or not 1 <= ttl <= 30 * 86400:
            raise StateError("state operation failed")
        expiry = time.time() + ttl
        raw = json.dumps({"value": value, "expires_at": expiry}, separators=(",", ":"), allow_nan=False).encode()
        if len(raw) > MAX_VALUE:
            raise StateError("state operation failed")
        nonce = os.urandom(12)
        encrypted = nonce + self.cipher.encrypt(nonce, raw, aad)
        self.cursor.execute("SELECT count(*), COALESCE(sum(octet_length(ciphertext)), 0) FROM headroom.entries WHERE scope = %s AND NOT (kind = %s AND id = %s)",
                            (self.scope, kind, identifier))
        count, size = self.cursor.fetchone()
        if count >= MAX_SCOPE_ENTRIES or size + len(encrypted) > MAX_SCOPE_BYTES:
            raise StateError("state operation failed")
        self.cursor.execute("""INSERT INTO headroom.entries (scope, kind, id, ciphertext, expires_at)
            VALUES (%s, %s, %s, %s, to_timestamp(%s))
            ON CONFLICT (scope, kind, id) DO UPDATE SET ciphertext = EXCLUDED.ciphertext, expires_at = EXCLUDED.expires_at""",
                            (self.scope, kind, identifier, encrypted, expiry))

    def delete(self, kind, identifier):
        self.aad(kind, identifier)
        self.cursor.execute("DELETE FROM headroom.entries WHERE scope = %s AND kind = %s AND id = %s", (self.scope, kind, identifier))
        return self.cursor.rowcount == 1

    def list(self, kind):
        if kind not in KINDS:
            raise StateError("state operation failed")
        self.cursor.execute("SELECT id FROM headroom.entries WHERE scope = %s AND kind = %s AND expires_at > now() ORDER BY id LIMIT %s",
                            (self.scope, kind, MAX_SCOPE_ENTRIES))
        identifiers = [row[0] for row in self.cursor.fetchall()]
        return [(identifier, self.get(kind, identifier)) for identifier in identifiers]
