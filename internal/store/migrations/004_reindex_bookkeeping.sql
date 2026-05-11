CREATE TABLE reindex_files (
    path             TEXT PRIMARY KEY,
    mtime            TEXT NOT NULL,
    content_hash     TEXT NOT NULL,
    last_indexed_at  TEXT NOT NULL,
    entity_id        TEXT NOT NULL,
    entity_kind      TEXT NOT NULL
);
CREATE INDEX idx_reindex_files_entity ON reindex_files(entity_id);
