// Reproduction harness for #384: does an operator-initiated fill fire a
// `fill_completed`-triggered workflow? Drives a real POST
// /v1/entities/{id}/fill through the same eventbus the workflow engine
// subscribes to (the production wiring: one shared bus) and observes,
// via a recording runner, whether the matching workflow fires.

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// recordingFillRunner captures which workflows the engine dispatches,
// so the test can assert a fill_completed trigger actually fired.
type recordingFillRunner struct {
	mu    sync.Mutex
	fired []string
}

func (r *recordingFillRunner) Run(_ context.Context, wf *parser.Workflow, _ actions.Decision, _ actions.Activation) []actions.ActionResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fired = append(r.fired, wf.Name)
	return nil
}

func (r *recordingFillRunner) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.fired...)
}

// TestRepro384_OperatorFill_FiresFillCompletedTrigger is the #384
// settle-it test: an operator-fill that resolves a workflow-injected
// gap on a non-canonical (gmail) entity must fire the matching
// `fill_completed` workflow — the same as the auto-fill path.
func TestRepro384_OperatorFill_FiresFillCompletedTrigger(t *testing.T) {
	t.Parallel()

	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	rd, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	// One shared bus — exactly the production wiring (main.go: a single
	// MemoryBus handed to both the api handlers and the engine).
	bus := eventbus.NewMemoryBus()

	runner := &recordingFillRunner{}
	resolver := &triggerFakeResolver{entities: map[string]map[string]any{
		"gmail:msg-1": {"id": "gmail:msg-1", "kind": "gmail"},
	}}
	eng, err := engine.New(engine.Options{
		Bus:      bus,
		Resolver: resolver,
		Runner:   runner,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	// The auto-archive-shaped workflow from the issue's repro: fires on
	// a fill that resolves the `is_actionable` gap.
	wf := &parser.Workflow{
		Name:           "gmail-not-actionable-archive",
		Version:        1,
		Status:         parser.StatusActive,
		AllowedPlugins: []string{"yaad-gmail"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeFillCompleted,
			Match: parser.TriggerMatch{Gap: "is_actionable"},
		},
		Subject: "entity.id",
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'observed'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, rd),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithEventBus(bus),
		WithWorkflowEngine(eng),
	)

	// Seed a gmail entity carrying a workflow-INJECTED gap
	// (is_actionable in gap_state + the open-gap list), mirroring what a
	// workflow `add_gap` action would have left on it. No canonical
	// kindCfg — gmail is a plugin source kind.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID:   "gmail:msg-1",
		Kind: "gmail",
		GapState: map[string]store.GapStateEntry{
			"is_actionable": {},
		},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID:     "gmail:msg-1",
		Kind:   "gmail",
		Source: []string{"yaad-gmail/default"},
		Data:   map[string]any{},
		Gaps:   []string{"is_actionable"},
		GapState: map[string]vault.GapStateEntry{
			"is_actionable": {},
		},
	}))

	// Operator-fill the gap (Subject == Operator == operator-strategy).
	tok := mintOperatorToken(t, signer, "alice")
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/gmail:msg-1/fill", tok,
		map[string]any{"is_actionable": "no"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, "operator-fill must succeed; body=%s", rec.Body.String())

	// Let the engine drain the published FillCompletedEvent.
	eng.WaitForIdle()

	fired := runner.snapshot()
	assert.Contains(t, fired, "gmail-not-actionable-archive",
		"#384: operator-fill on an injected gap must fire the fill_completed workflow; fired=%v", fired)
}
