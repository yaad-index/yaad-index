package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// fetchAttachmentJSON is the on-disk shape for a single
// FetchAttachmentRef inside the provenance.fetch_attachments JSON
// column. Mirrors FetchAttachmentRef field-for-field with
// json-tagged keys; kept private so the column shape is opaque
// outside this file.
type fetchAttachmentJSON struct {
	Role string `json:"role"`
	URI string `json:"uri"`
}

// marshalFetchAttachments serializes a FetchAttachmentRef slice for
// the `fetch_attachments` column. Empty / nil → sql.NullString with
// Valid=false (NULL on the row), so backward-compat with pre-ADR-0014
// rows is preserved (they read as nil; we write NULL when there are
// no attachments).
//
// Returning a value the driver knows how to bind is the simpler shape
// than juggling sql.NullString at every call site — we use
// sql.NullString here so SQLite stores NULL rather than the JSON
// literal "null" or an empty string (which would round-trip back as
// `[]` if anyone ever decodes it loosely).
func marshalFetchAttachments(refs []FetchAttachmentRef) (sql.NullString, error) {
	if len(refs) == 0 {
		return sql.NullString{}, nil
	}
	rows := make([]fetchAttachmentJSON, len(refs))
	for i, r := range refs {
		rows[i] = fetchAttachmentJSON(r)
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal fetch_attachments: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// unmarshalFetchAttachments decodes the `fetch_attachments` column
// back into a []FetchAttachmentRef. Empty / "[]" inputs return nil
// (matches the marshal NULL contract). Malformed JSON returns an
// error; the caller logs + degrades to "no prior attachments" — the
// next ingest performs every fetch unconditionally, which is correct
// when the column is unparseable.
func unmarshalFetchAttachments(raw string) ([]FetchAttachmentRef, error) {
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var rows []fetchAttachmentJSON
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, fmt.Errorf("unmarshal fetch_attachments: %w", err)
	}
	out := make([]FetchAttachmentRef, len(rows))
	for i, r := range rows {
		out[i] = FetchAttachmentRef(r)
	}
	return out, nil
}
