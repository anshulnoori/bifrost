-- Run as headroom on its dedicated database, also owned by headroom.
-- Create the role and set its password through the private secret workflow first.
-- The Modal runtime does not receive the Bifrost database role or encryption key.
BEGIN;
CREATE SCHEMA IF NOT EXISTS headroom;
REVOKE ALL ON SCHEMA headroom FROM PUBLIC;
CREATE TABLE IF NOT EXISTS headroom.entries (
    scope char(64) NOT NULL,
    kind text NOT NULL CHECK (kind IN ('ccr', 'memory', 'learning', 'operation')),
    id varchar(64) NOT NULL,
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) <= 4194332),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (scope, kind, id)
);
CREATE INDEX IF NOT EXISTS headroom_entries_expiry ON headroom.entries (expires_at);
ALTER ROLE headroom SET statement_timeout = '5s';
ALTER ROLE headroom SET lock_timeout = '2s';
ALTER ROLE headroom SET idle_in_transaction_session_timeout = '5s';
COMMIT;

-- Run periodically using an owner-managed scheduled task. Reads reject expired
-- records even before this physical deletion. Backups follow the Neon policy.
-- DELETE FROM headroom.entries WHERE expires_at <= now();
