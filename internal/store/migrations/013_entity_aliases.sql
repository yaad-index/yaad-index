-- entity_aliases is the alias-to-entity lookup table per #3.
-- Mirrors entity_notations' shape so the search layer can JOIN
-- both axes the same way + the reindex re-derive uses the same
-- DELETE+INSERT pattern.
--
-- alias       — full alias text. PRIMARY KEY because v1 treats
--               aliases as unique pointers to a single entity
--               (same trade-off entity_notations made). Real
--               cross-entity collisions are rare; if operator
--               pain surfaces, a future migration switches to
--               (alias, entity_id) compound PK.
-- entity_id   — the entity slug this alias points at (FK on
--               entities, ON DELETE CASCADE so reindex's vault-
--               wins teardown doesn't leave orphan rows).
-- alias_kind  — discriminator in the closed set {'bare',
--               'typed'} per #3 §"Bare-string AND typed-prefix
--               shapes". 'bare' is the default + the Obsidian
--               wikilink target shape. 'typed' marks an alias
--               whose `<edge-type>: <label>` prefix matches an
--               operator-declared canonical_edge_types entry —
--               agents reverse-lookup-filter on this.
--               The kind is derived at ReplaceAliases time by
--               the ingest path (which has the operator's
--               canonical_edge_types in scope); the store
--               itself doesn't enforce the registry constraint.
CREATE TABLE entity_aliases (
    alias       TEXT NOT NULL PRIMARY KEY,
    entity_id   TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    alias_kind  TEXT NOT NULL DEFAULT 'bare'
);

-- Reverse-lookup index: "give me every alias pointing at this
-- entity" — used by reindex when it re-derives the table from
-- vault frontmatter (DELETE-then-INSERT-by-entity_id, same
-- shape as entity_notations).
CREATE INDEX idx_entity_aliases_entity_id ON entity_aliases(entity_id);
