package vault

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewNoteID returns an 8-hex-char random note identifier per ADR-0015
// §Note identity (#390). 4 bytes of crypto/rand → 8 hex chars: ample
// collision resistance at per-entity note scale, short enough to read
// in vault diffs + logs. Errors only on a catastrophic CSPRNG failure
// (no entropy) — callers surface it rather than stamping a weak / empty
// id that would break edit/delete targeting.
//
// Lives in the vault package (not a writer) so every note-creation path
// — the API add_note handler and the workflow add_note action — shares
// one id source. Serialization (Marshal) deliberately does NOT call
// this: id stamping is an explicit caller step so Marshal stays
// deterministic.
func NewNoteID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("note id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// EnsureNoteIDs stamps a fresh id, in place, on every note lacking one
// — the ADR-0015 §Note identity back-compat path: legacy id-less notes
// acquire an id whenever a note operation next writes the block.
func EnsureNoteIDs(notes []Note) error {
	for i := range notes {
		if notes[i].ID == "" {
			id, err := NewNoteID()
			if err != nil {
				return err
			}
			notes[i].ID = id
		}
	}
	return nil
}
