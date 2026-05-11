-- Per-plugin counters for canonical-shape emissions the operator's
-- config dropped (per ADR-0013 §3 / yaad-index #175 PR-1). When a
-- plugin emits a canonical entity stub or canonical edge type that
-- isn't enabled in `canonical_kinds:` / `canonical_edge_types:`,
-- the orchestrator drops the emission and increments the matching
-- row here. `GET /v1/cv-status` (PR-2) reads this table to surface
-- the drift signal — concrete "you have N entities you would have
-- materialized if you enabled kind X."
--
-- Today the only signal is startup-time WARN logs (per ADR-0013 §3
-- "the only signal is startup WARN logs ... logs scroll, operators
-- don't see the drift after that"). This counter persists across
-- daemon restart so the surface reflects actual workload state.
--
-- Two tables, one per axis (kinds vs edge-types). Same shape
-- (plugin, name, count, timestamps); separated so the schema's
-- self-documenting and a future axis-specific column doesn't have
-- to bolt onto a single union table.
--
-- (plugin, kind) / (plugin, edge_type) is the natural composite
-- key — one plugin emits a kind name once, but two plugins
-- emitting the same kind get separate rows. PRIMARY KEY enforces
-- the uniqueness; INSERT...ON CONFLICT DO UPDATE bumps the count.

-- DEFAULT 1 (not 0): every row exists because at least one drop
-- has happened — that's the entire point of the table. A bare
-- INSERT (plugin, kind) without explicit count produces a valid
-- "first observation" row rather than a silently-wrong zero row
-- (sora's PR-176 catch). The application path always passes
-- explicit 1 via the IncDroppedCanonical* helpers so the default
-- is defense-in-depth, not the live path.

CREATE TABLE dropped_canonical_kinds (
    plugin       TEXT NOT NULL,
    kind         TEXT NOT NULL,
    count        INTEGER NOT NULL DEFAULT 1,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    PRIMARY KEY (plugin, kind)
);

CREATE TABLE dropped_canonical_edges (
    plugin       TEXT NOT NULL,
    edge_type    TEXT NOT NULL,
    count        INTEGER NOT NULL DEFAULT 1,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    PRIMARY KEY (plugin, edge_type)
);
