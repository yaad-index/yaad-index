package plugins

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCapabilities_ResolvesCanonicalKinds_WireRoundTrip pins the
// JSON wire shape per #304 Cut A: `resolves_canonical_kinds` field
// is omitempty (predating plugins emit nothing), and the decode
// path lifts the array into the Go field for the daemon's
// resolver-routing map.
func TestCapabilities_ResolvesCanonicalKinds_WireRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("encode with claim", func(t *testing.T) {
		t.Parallel()
		c := Capabilities{
			Name:                   "wikipedia",
			CanonicalKindsEmitted:  []string{"person", "city"},
			ResolvesCanonicalKinds: []string{"person", "city"},
		}
		b, err := json.Marshal(c)
		require.NoError(t, err)
		assert.Contains(t, string(b), `"resolves_canonical_kinds":["person","city"]`)
	})

	t.Run("encode without claim — omitempty drops the field", func(t *testing.T) {
		t.Parallel()
		c := Capabilities{
			Name:                  "legacy",
			CanonicalKindsEmitted: []string{"person"},
		}
		b, err := json.Marshal(c)
		require.NoError(t, err)
		assert.NotContains(t, string(b), `"resolves_canonical_kinds"`,
			"predating plugins must not stamp an empty resolves_canonical_kinds on the wire")
	})

	t.Run("decode plugin-emitted document", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"name": "bgg",
			"canonical_kinds_emitted": ["boardgame", "person"],
			"resolves_canonical_kinds": ["boardgame"]
		}`
		var got Capabilities
		require.NoError(t, json.Unmarshal([]byte(raw), &got))
		assert.Equal(t, []string{"boardgame"}, got.ResolvesCanonicalKinds)
	})

	t.Run("decode legacy plugin doc — field absent", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"name": "legacy",
			"canonical_kinds_emitted": ["person"]
		}`
		var got Capabilities
		require.NoError(t, json.Unmarshal([]byte(raw), &got))
		assert.Nil(t, got.ResolvesCanonicalKinds,
			"legacy plugins decode to nil — empty slice would falsely signal opt-in")
	})
}
