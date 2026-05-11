CREATE TABLE entities (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    data         TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX idx_entities_kind ON entities(kind);

CREATE TABLE edges (
    type         TEXT NOT NULL,
    from_id      TEXT NOT NULL REFERENCES entities(id),
    to_id        TEXT NOT NULL REFERENCES entities(id),
    metadata     TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (type, from_id, to_id)
);
CREATE INDEX idx_edges_from ON edges(from_id);
CREATE INDEX idx_edges_to   ON edges(to_id);

CREATE TABLE provenance (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    target_kind      TEXT NOT NULL CHECK (target_kind IN ('entity', 'edge')),
    target_entity_id TEXT,
    target_edge_type TEXT,
    target_edge_from TEXT,
    target_edge_to   TEXT,
    source           TEXT NOT NULL,
    fetched_at       TEXT,
    filled_at        TEXT,
    ok               INTEGER NOT NULL,
    error            TEXT,
    error_message    TEXT
);
CREATE INDEX idx_prov_entity ON provenance(target_entity_id);
CREATE INDEX idx_prov_edge   ON provenance(target_edge_type, target_edge_from, target_edge_to);
CREATE INDEX idx_prov_source ON provenance(source);
