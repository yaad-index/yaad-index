-- entity_notations is the notation-to-slug lookup table per yaad-
-- index issue #120 (PR-1). Maps every valid input form (full URL,
-- shorthand `<plugin>: <id>`, future input shapes) to the canonical
-- entity slug it resolves to. Lets the orchestrator skip the upstream
-- plugin Fetch on cache hits while accepting any of the equivalent
-- notations a caller might pass in.
--
-- This migration adds the table only — the lookup-first ingest flow
-- and reindex re-derive land in PR-3 / PR-4. The store API ships in
-- this same PR.
--
-- notation       — exact input string. PRIMARY KEY because the same
--                  notation can never resolve to two entities; the
--                  inverse (one entity reachable via many notations)
--                  is the whole point of the table.
-- entity_id      — the slug the notation resolves to (FK on entities).
-- notation_kind  — discriminator (e.g. `url`, `shorthand`). Used by
--                  reindex semantics so a vault re-derive can decide
--                  how to format the notation back into frontmatter.
--
-- ON DELETE CASCADE: a deleted entity's notations die with it. Vault
-- reindex's DeleteEntityCascade path (operator deletion of a vault
-- file) needs this to keep notations from outliving the entity they
-- point at.
CREATE TABLE entity_notations (
    notation       TEXT NOT NULL PRIMARY KEY,
    entity_id      TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    notation_kind  TEXT NOT NULL DEFAULT 'url'
);

-- Reverse-lookup index: "give me every notation pointing at this
-- entity" — used by reindex when it re-derives the table from vault
-- frontmatter (DELETE-then-INSERT-by-entity_id).
CREATE INDEX idx_entity_notations_entity_id ON entity_notations(entity_id);
