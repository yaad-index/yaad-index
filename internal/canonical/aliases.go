package canonical

import (
	"context"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// MirrorAliases recomputes the synthesized + plugin-emitted alias set
// for an entity (via vault.MergedAliasesFor, the same merge Marshal
// performs when writing the vault frontmatter) and writes it to the
// DB `entity_aliases` index so the aliases resolve immediately —
// without waiting for the next reindex/ingest pass to backfill them
// from frontmatter.
//
// This is the shared "register the source-of-slug name" primitive
// behind #405: the three first-class creation surfaces (canonical
// thin-edge materialize, create_canonical_entity, UGC create) each
// write the vault file with aliases, then call this to mirror those
// aliases into the resolver index in the same request.
//
// canonicalKinds selects the title field synthesizeAliases reads:
// source-shape entities (kind NOT in the set) alias off data.title,
// canonical-shape entities (kind in the set) off data.name. Pass the
// operator's full canonical_kinds set — NOT just the entity's own
// kind — so a source-shape entity (e.g. user-content) correctly falls
// to the data.title branch.
//
// An empty merged set (no title/name, or the candidate equals the
// slug) writes an empty alias list, which ReplaceAliases treats as a
// clear — correct for a fresh create (no prior aliases to wipe).
//
// canonicalEdgeTypes is the operator's registered edge-type set used to
// derive each alias's Kind (typed vs bare) via store.TypedAliasEntries,
// matching the reindex + ingest paths. Without it every mirrored alias
// would be written bare, miscategorizing typed aliases on the #405
// creation surfaces until the next reindex pass (#445).
func MirrorAliases(ctx context.Context, st store.Store, id, kind string, data map[string]any, pluginAliases, canonicalKinds, canonicalEdgeTypes []string) error {
	merged := vault.MergedAliasesFor(id, kind, data, pluginAliases, canonicalKinds)
	return st.ReplaceAliases(ctx, id, store.TypedAliasEntries(id, merged, canonicalEdgeTypes))
}
