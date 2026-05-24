// Tests for the vault-edges-as-source-of-truth contract on the
// ingest-write side: buildVaultEntity threads
// `result.CanonicalEdges` into `vault.Entity.Edges` so the vault
// frontmatter carries the resolved canonical-label edges. Reindex
// then reconstitutes the DB from the vault block alone.

package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// minimalStoreEntity is a one-liner fixture for the buildVaultEntity
// tests below — every test wants id+kind populated, nothing else.
func minimalStoreEntity(id, kind string) *store.Entity {
	return &store.Entity{ID: id, Kind: kind}
}

func TestBuildVaultEntity_CanonicalEdgesPopulateVaultEdges(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	prov := store.ProvenanceEntry{
		Source: "bgg",
		FetchedAt: &now,
		OK: true,
	}
	canonicalEdges := []*store.Edge{
		{Type: "is_about", From: "bgg:age-of-steam", To: "boardgame:age-of-steam"},
		{Type: "is_a", From: "bgg:age-of-steam", To: "source-type:bgg-record"},
		{Type: "designed_by", From: "bgg:age-of-steam", To: "person:john-bohrer"},
		{Type: "designed_by", From: "bgg:age-of-steam", To: "person:martin-wallace"},
	}

	got := buildVaultEntity(
		minimalStoreEntity("bgg:age-of-steam", "bgg"),
		[]string{"bgg/default"},
		nil, // gaps
		"", // cleanContent
		nil, // notations
		nil, // aliases
		prov,
		nil, // existing
		nil, // cacheExpires
		nil, // attachmentManifest
		canonicalEdges,
	)

	require.Len(t, got.Edges, 4, "all four canonical edges land on vault.Entity.Edges")

	// Order preserved from the canonicalEdges input. Each entry
	// carries Type+To only — vault.Edge has no From (implicit on
	// the source entity) and no Provenance (lives on the entity's
	// own provenance list).
	assert.Equal(t, vault.Edge{Type: "is_about", To: "boardgame:age-of-steam"}, got.Edges[0])
	assert.Equal(t, vault.Edge{Type: "is_a", To: "source-type:bgg-record"}, got.Edges[1])
	assert.Equal(t, vault.Edge{Type: "designed_by", To: "person:john-bohrer"}, got.Edges[2])
	assert.Equal(t, vault.Edge{Type: "designed_by", To: "person:martin-wallace"}, got.Edges[3])
}

func TestBuildVaultEntity_NilCanonicalEdgesPreservesExisting(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	prov := store.ProvenanceEntry{Source: "bgg", FetchedAt: &now, OK: true}

	existing := &vault.Entity{
		ID: "bgg:age-of-steam",
		Kind: "bgg",
		Edges: []vault.Edge{
			{Type: "is_about", To: "boardgame:age-of-steam"},
			{Type: "designed_by", To: "person:john-bohrer"},
		},
	}

	// nil canonicalEdges = "this ingest didn't emit edges" (e.g.
	// fixture path or a plugin that doesn't carry canonical-edge
	// shape on its FetchResult). Existing edges from the prior
	// vault file MUST survive — a non-edge plugin's re-ingest
	// must not wipe an edge set written by an earlier plugin run.
	got := buildVaultEntity(
		minimalStoreEntity("bgg:age-of-steam", "bgg"),
		[]string{"bgg/default"},
		nil, "", nil, nil, prov, existing, nil, nil,
		nil, // canonicalEdges == nil
	)

	assert.Equal(t, existing.Edges, got.Edges,
		"nil canonicalEdges leaves existing.Edges intact")
}

func TestBuildVaultEntity_EmptyCanonicalEdgesClearsExisting(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	prov := store.ProvenanceEntry{Source: "bgg", FetchedAt: &now, OK: true}

	existing := &vault.Entity{
		ID: "bgg:age-of-steam",
		Kind: "bgg",
		Edges: []vault.Edge{
			{Type: "designed_by", To: "person:john-bohrer"},
		},
	}

	// Empty (non-nil) canonicalEdges = "this ingest re-emitted with
	// zero edges". The plugin's view is canonical for source-shape
	// edges, so the prior set drops. Distinct from the nil-preserves
	// path above: nil is "no edge data on this fetch", empty is "no
	// edges in the new fetch".
	got := buildVaultEntity(
		minimalStoreEntity("bgg:age-of-steam", "bgg"),
		[]string{"bgg/default"},
		nil, "", nil, nil, prov, existing, nil, nil,
		[]*store.Edge{}, // empty (not nil)
	)

	assert.Empty(t, got.Edges,
		"empty canonicalEdges replaces existing.Edges with zero-len slice")
}

func TestBuildVaultEntity_CanonicalEdgesSkipsMalformedEntries(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	prov := store.ProvenanceEntry{Source: "bgg", FetchedAt: &now, OK: true}

	// Defensive: a misbehaving plugin emitting partial entries
	// (empty Type or empty To) must not produce malformed vault
	// frontmatter. The good entries land; the bad ones drop.
	canonicalEdges := []*store.Edge{
		{Type: "is_about", From: "bgg:foo", To: "boardgame:foo"}, // good
		{Type: "", From: "bgg:foo", To: "person:bad-no-type"}, // dropped
		{Type: "designed_by", From: "bgg:foo", To: ""}, // dropped
		nil, // dropped
	}

	got := buildVaultEntity(
		minimalStoreEntity("bgg:foo", "bgg"),
		[]string{"bgg/default"},
		nil, "", nil, nil, prov, nil, nil, nil, canonicalEdges,
	)

	require.Len(t, got.Edges, 1, "malformed entries should drop")
	assert.Equal(t, vault.Edge{Type: "is_about", To: "boardgame:foo"}, got.Edges[0])
}
