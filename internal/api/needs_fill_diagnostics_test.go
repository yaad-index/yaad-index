package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// TestDiagnoseNeedsFill_Buckets pins the #523 classifier: an entity whose
// only open, non-deferred, shaped gaps are fill_strategy=operator is
// OperatorOnly (hidden from agents — the suspected cause); agent/either
// gaps are AgentCallable; all-deferred is HiddenFromBoth; etc.
func TestDiagnoseNeedsFill_Buckets(t *testing.T) {
	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	// Gap shapes are workflow-injected (#142) directly in gap_state, so
	// the classifier resolves fill_strategy without an operator registry.
	write := func(id, gap string, entry vault.GapStateEntry) {
		gaps := []string{}
		gs := map[string]vault.GapStateEntry{}
		if gap != "" {
			gaps = append(gaps, gap)
			gs[gap] = entry
		}
		require.NoError(t, w.Write(&vault.Entity{
			ID:       id,
			Kind:     "boardgame",
			Source:   []string{"seed/default"},
			Data:     map[string]any{"id": id},
			Gaps:     gaps,
			GapState: gs,
		}))
	}

	write("boardgame:op-only", "rating", vault.GapStateEntry{Type: "int", FillStrategy: "operator"})
	write("boardgame:agent", "notes", vault.GapStateEntry{Type: "string", FillStrategy: "agent"})
	write("boardgame:either", "free", vault.GapStateEntry{Type: "string"}) // no strategy → both
	write("boardgame:deferred", "later", vault.GapStateEntry{Type: "string", FillStrategy: "agent", Deferred: true})
	write("boardgame:nogaps", "", vault.GapStateEntry{})

	entities := []store.Entity{
		{ID: "boardgame:op-only", Kind: "boardgame"},
		{ID: "boardgame:agent", Kind: "boardgame"},
		{ID: "boardgame:either", Kind: "boardgame"},
		{ID: "boardgame:deferred", Kind: "boardgame"},
		{ID: "boardgame:nogaps", Kind: "boardgame"},
		{ID: "boardgame:vault-missing", Kind: "boardgame"}, // never written to vault
	}

	d := DiagnoseNeedsFill(map[string]config.CanonicalKindConfig{}, r, entities)

	assert.Equal(t, 6, d.TotalEntities)
	assert.Equal(t, 1, d.VaultMissing, "vault-missing")
	assert.Equal(t, 1, d.NoOpenGaps, "no-open-gaps")
	assert.Equal(t, 2, d.AgentCallable, "agent + either")
	assert.Equal(t, 1, d.OperatorOnly, "operator-only")
	assert.Equal(t, 1, d.HiddenFromBoth, "all-deferred")
	assert.Contains(t, d.OperatorOnlySample, "boardgame:op-only")
}
