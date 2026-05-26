package api

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yaad-index/yaad-index/internal/store"
)

// TestBuildAliasEntries_NilInputReturnsNil pins the empty-input
// shape so ReplaceAliases sees a nil slice (clears the entity's
// rows).
func TestBuildAliasEntries_NilInputReturnsNil(t *testing.T) {
	t.Parallel()

	assert.Nil(t, buildAliasEntries("book:piranesi", nil, []string{"author"}))
	assert.Nil(t, buildAliasEntries("book:piranesi", []string{}, []string{"author"}))
}

// TestBuildAliasEntries_BareAndTypedClassification pins the
// alias_kind discriminator: a registered prefix → typed, an
// unregistered prefix → bare even with the `<prefix>: <label>`
// shape, a non-prefixed string → bare.
func TestBuildAliasEntries_BareAndTypedClassification(t *testing.T) {
	t.Parallel()

	got := buildAliasEntries("book:piranesi", []string{
		"Piranesi",
		"author: Susanna Clarke",
		"isbn: 9781635575637",
		"unknown: prefix",
	}, []string{"author", "isbn"})

	assert.Equal(t, []store.Alias{
		{Alias: "Piranesi", EntityID: "book:piranesi", Kind: store.AliasKindBare},
		{Alias: "author: Susanna Clarke", EntityID: "book:piranesi", Kind: store.AliasKindTyped},
		{Alias: "isbn: 9781635575637", EntityID: "book:piranesi", Kind: store.AliasKindTyped},
		{Alias: "unknown: prefix", EntityID: "book:piranesi", Kind: store.AliasKindBare},
	}, got)
}

// TestBuildAliasEntries_DedupesAndSkipsEmpty pins the defensive
// shape: within-batch duplicates are first-occurrence-wins, empty
// strings are dropped (don't fail the ingest).
func TestBuildAliasEntries_DedupesAndSkipsEmpty(t *testing.T) {
	t.Parallel()

	got := buildAliasEntries("book:piranesi", []string{
		"Piranesi",
		"",
		"Piranesi",
		"",
		"Clarke",
	}, nil)

	assert.Equal(t, []store.Alias{
		{Alias: "Piranesi", EntityID: "book:piranesi", Kind: store.AliasKindBare},
		{Alias: "Clarke", EntityID: "book:piranesi", Kind: store.AliasKindBare},
	}, got)
}

// TestBuildAliasEntries_NilEdgeTypesPermissive pins the dev-binary
// shape: a nil canonicalEdgeTypes registry never classifies as
// typed — every alias bare.
func TestBuildAliasEntries_NilEdgeTypesPermissive(t *testing.T) {
	t.Parallel()

	got := buildAliasEntries("book:piranesi", []string{
		"Piranesi",
		"author: Susanna Clarke",
	}, nil)

	assert.Equal(t, []store.Alias{
		{Alias: "Piranesi", EntityID: "book:piranesi", Kind: store.AliasKindBare},
		{Alias: "author: Susanna Clarke", EntityID: "book:piranesi", Kind: store.AliasKindBare},
	}, got)
}
