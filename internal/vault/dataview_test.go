package vault

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDataview_MarshalUnmarshalRoundTrip pins yaad-index #119:
// a vault.Entity with Dataview paragraphs round-trips through
// Marshal → Unmarshal preserving the field set per paragraph.
// Sorted-key render is the dedup contract.
func TestDataview_MarshalUnmarshalRoundTrip(t *testing.T) {
	t.Parallel()

	want := &Entity{
		ID:     "company:parloa",
		Kind:   "company",
		Plugin: "user",
		Dataview: []DataviewParagraph{
			{Fields: map[string]string{
				"role":         "Staff Platform Engineer",
				"salary":       "150k+",
				"work_mode":    "hybrid",
				"source_email": "gmail:linkedin-alert-1",
			}},
			{Fields: map[string]string{
				"role":      "Senior Engineer",
				"salary":    "130k+",
				"work_mode": "remote",
			}},
		},
	}
	b, err := Marshal(want, nil)
	require.NoError(t, err)
	out := string(b)

	// Both markers present + paragraph lines inside.
	assert.Contains(t, out, DataviewStartMarker,
		"marshalled body must include the dataview start marker")
	assert.Contains(t, out, DataviewEndMarker,
		"marshalled body must include the dataview end marker")
	// Sorted-key render: alphabetical field order regardless of
	// the input map's iteration order. The first paragraph's
	// keys are `role, salary, source_email, work_mode` sorted.
	assert.Contains(t, out, "role:: Staff Platform Engineer  salary:: 150k+  source_email:: gmail:linkedin-alert-1  work_mode:: hybrid",
		"first paragraph must render sorted-key")

	got, err := Unmarshal(b)
	require.NoError(t, err)
	require.Len(t, got.Dataview, 2,
		"both paragraphs survive round-trip")
	assert.Equal(t, want.Dataview[0].Fields, got.Dataview[0].Fields,
		"paragraph 0 fields round-trip")
	assert.Equal(t, want.Dataview[1].Fields, got.Dataview[1].Fields,
		"paragraph 1 fields round-trip")
}

// TestDataview_EmptyOmitsSection pins the no-noise contract:
// an entity with no Dataview paragraphs writes no marker pair.
func TestDataview_EmptyOmitsSection(t *testing.T) {
	t.Parallel()

	e := &Entity{ID: "company:parloa", Kind: "company", Plugin: "user"}
	b, err := Marshal(e, nil)
	require.NoError(t, err)
	out := string(b)
	assert.NotContains(t, out, DataviewStartMarker,
		"no markers when there are zero paragraphs")
	assert.NotContains(t, out, DataviewEndMarker,
		"no markers when there are zero paragraphs")
}

// TestDataview_BareKeyValueLineStaysInCleanContent pins the
// marker-only contract (same lesson as the notes-marker
// collision fix): `key:: value` prose OUTSIDE the marker pair
// is normal user content and lands in CleanContent unchanged.
// Dataview-mode activates strictly on the marker.
func TestDataview_BareKeyValueLineStaysInCleanContent(t *testing.T) {
	t.Parallel()

	body := strings.TrimSpace(`
---
id: company:parloa
kind: company
plugin: user
---

I'm tracking these manually:

note:: some prose about something
`) + "\n"

	parsed, err := Unmarshal([]byte(body))
	require.NoError(t, err)
	assert.Empty(t, parsed.Dataview,
		"bare key:: value prose must NOT activate dataview-mode without the marker pair")
	assert.Contains(t, parsed.CleanContent, "note:: some prose",
		"bare key:: value prose must round-trip in CleanContent")
}

// TestDataview_MarkerWrappedRoundTrip pins the explicit-
// marker read path: a body wrapped in markers parses to
// paragraphs even if it has no other yaad sections.
func TestDataview_MarkerWrappedRoundTrip(t *testing.T) {
	t.Parallel()

	body := strings.TrimSpace(`
---
id: company:parloa
kind: company
plugin: user
---

` + DataviewStartMarker + `
role:: Staff Engineer  salary:: 140k
` + DataviewEndMarker + `
`) + "\n"

	parsed, err := Unmarshal([]byte(body))
	require.NoError(t, err)
	require.Len(t, parsed.Dataview, 1)
	assert.Equal(t, "Staff Engineer", parsed.Dataview[0].Fields["role"])
	assert.Equal(t, "140k", parsed.Dataview[0].Fields["salary"])
}

// TestRenderDataviewParagraph_DeterministicOrder pins the
// sorted-key contract from the public helper — handlers use
// this same render to compute dedup keys.
func TestRenderDataviewParagraph_DeterministicOrder(t *testing.T) {
	t.Parallel()

	p := DataviewParagraph{Fields: map[string]string{
		"z_last":  "z",
		"a_first": "a",
		"m_mid":   "m",
	}}
	assert.Equal(t,
		"a_first:: a  m_mid:: m  z_last:: z",
		RenderDataviewParagraph(p))
}
