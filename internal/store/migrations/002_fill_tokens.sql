CREATE TABLE fill_tokens (
    token       TEXT PRIMARY KEY,
    entity_id   TEXT NOT NULL,
    gaps        TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT
);
CREATE INDEX idx_fill_tokens_entity ON fill_tokens(entity_id);
CREATE INDEX idx_fill_tokens_expires ON fill_tokens(expires_at);
