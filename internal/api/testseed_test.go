package api

import (
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
)

// testSeedKinds is the canonical list of (entity kind, edge kind)
// pairs the test suite assumes is registered. It mirrors what the
// retired bootstrapKinds seed served — boardgame / book / person plus
// designed_by / authored_by — so existing closure-invariant
// assertions in *_test.go keep working without churn.
//
// Two test plugins (`bgg` + `books`) carry these between them, so
// source_plugins on /v1/kinds reflects which fixture contributed what
// (matching the historical seed). Tests asserting against the union
// pull from the slices below.
var (
	testSeedEntityKinds = []string{"boardgame", "book", "person"}
	testSeedEdgeKinds = []string{"authored_by", "designed_by"}
)

// testRegistryWithSeed returns a freshly-built plugins.Registry pre-
// populated with two fixture plugins whose Capabilities() advertise
// the historical seed kinds. Used by every test API helper that
// previously relied on the bootstrapKinds seed.
//
// The fixtures don't claim any URL — Match always returns false — so
// they never compete with real plugins for ingest dispatch in tests
// that register a third "real" plugin alongside.
func testRegistryWithSeed() *plugins.Registry {
	r := plugins.NewRegistry()
	r.Register(&fixture.Plugin{
		NameValue: "bgg",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "bgg",
			EntityKinds: []plugins.KindSpec{
				{Name: "boardgame", Description: "A boardgame as catalogued on BoardGameGeek."},
				{Name: "person", Description: "A designer, author, or contributor referenced by another entity."},
			},
			EdgeKinds: []plugins.KindSpec{
				{Name: "designed_by", Description: "Relates a boardgame to its designer(s).", FromKind: "boardgame", ToKind: "person"},
			},
		},
	})
	r.Register(&fixture.Plugin{
		NameValue: "books",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "books",
			EntityKinds: []plugins.KindSpec{
				{Name: "book", Description: "A book referenced by an authored_by edge."},
				// `person` overlaps with bgg above so source_plugins
				// dedup is exercised.
				{Name: "person", Description: "A designer, author, or contributor referenced by another entity."},
			},
			EdgeKinds: []plugins.KindSpec{
				{Name: "authored_by", Description: "Relates a book to its author(s).", FromKind: "book", ToKind: "person"},
			},
		},
	})
	return r
}
