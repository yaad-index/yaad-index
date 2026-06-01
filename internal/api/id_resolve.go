package api

import (
	"context"
	"errors"
	"strings"

	"github.com/yaad-index/yaad-index/internal/store"
)

// resolveEntityID resolves a user-supplied entity reference to a
// canonical entity id per #392 (alias-as-resolver). It is called only
// from the user-facing id-taking handlers; internal paths (reindex, the
// workflow resolver, the create-collision check, ingest) deliberately
// do NOT call it — they must operate on raw ids.
//
// Resolution order:
//
//  1. Exact id — if an entity with this id exists, use it (fast path,
//     unchanged behavior; an alias never shadows a real id).
//  2. Alias-by-kind — split "<kind>:<rest>" on the first colon and
//     reverse-look-up <rest> as an exact alias scoped to <kind>. On a
//     hit, return the matched canonical id.
//  3. No match — return the raw id unchanged, so the caller's existing
//     not_found path fires exactly as before.
//
// Fuzzy case-insensitive name-normalized matching + multi-candidate
// disambiguation are a deferred follow-up: exact aliases are globally
// unique, so the alias path here is never ambiguous.
func resolveEntityID(ctx context.Context, st store.Store, rawID string) (string, error) {
	if _, err := st.GetEntity(ctx, rawID); err == nil {
		return rawID, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	kind, rest, ok := strings.Cut(rawID, ":")
	if !ok || rest == "" {
		return rawID, nil
	}
	resolved, err := st.ResolveAlias(ctx, rest, kind)
	if err != nil {
		return "", err
	}
	if resolved != "" {
		return resolved, nil
	}
	return rawID, nil
}
