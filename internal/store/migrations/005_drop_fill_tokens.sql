-- Drops the fill_tokens table introduced in 002. The mechanism has
-- been unused since PR #57's vault-first fill rewrite (ADR-0008): the
-- entity ID is the durable callback, gaps live in vault frontmatter,
-- and the API surface no longer reads or writes this table.
--
-- IF EXISTS keeps the migration idempotent — DBs that never had the
-- table (anything bootstrapped after this migration lands in a fresh
-- environment) are unaffected.
DROP INDEX IF EXISTS idx_fill_tokens_entity;
DROP INDEX IF EXISTS idx_fill_tokens_expires;
DROP TABLE IF EXISTS fill_tokens;
