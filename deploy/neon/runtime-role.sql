-- Owner-operated after migration, on a new dedicated Bifrost database only.
-- Set the runtime password through Neon's masked role UI, never this file.
-- Replace role names only if your dedicated deployment uses different names.
BEGIN;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO bifrost_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO bifrost_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO bifrost_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE bifrost_migrator IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO bifrost_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE bifrost_migrator IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO bifrost_runtime;
ALTER ROLE bifrost_runtime SET statement_timeout = '30s';
ALTER ROLE bifrost_runtime SET lock_timeout = '5s';
ALTER ROLE bifrost_runtime SET idle_in_transaction_session_timeout = '30s';
COMMIT;
