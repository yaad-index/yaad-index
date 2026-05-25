package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/workflow/decision"
)

// TestStoreEntityResolver_NestsDataUnderDataKey pins the #145
// fix: the entity activation map carries `data` as a nested
// sub-map (not flattened to top-level), so CEL predicates
// reading `entity.data.<field>` resolve correctly. Before the
// fix the resolver flattened Data into top-level keys and
// every `entity.data.X` predicate raised `no such key: data`.
func TestStoreEntityResolver_NestsDataUnderDataKey(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "gmail:msg-x",
		Kind: "gmail",
		Data: map[string]any{
			"subject": "[acme/widget] PR #42 review requested",
			"date":    "2026-05-17T11:00:00Z",
		},
	}))

	r := &storeEntityResolver{st: st}
	got, err := r.Resolve(ctx, "gmail:msg-x")
	require.NoError(t, err)

	assert.Equal(t, "gmail:msg-x", got["id"], "id at top level")
	assert.Equal(t, "gmail", got["kind"], "kind at top level")

	data, ok := got["data"].(map[string]any)
	require.True(t, ok, "data must be a nested map[string]any, not flattened to top level")
	assert.Equal(t, "[acme/widget] PR #42 review requested", data["subject"])
	assert.Equal(t, "2026-05-17T11:00:00Z", data["date"])

	// Pre-fix behavior would have placed `subject` + `date` at
	// top level (`got["subject"]` non-nil). The fix ensures
	// they are NOT at top level — workflows that incorrectly
	// referenced `entity.subject` before would have worked by
	// accident; the documented pattern is `entity.data.subject`
	// and that's what now succeeds.
	_, subjectAtTop := got["subject"]
	assert.False(t, subjectAtTop, "data fields must not leak to top level")
}

// TestStoreEntityResolver_EmptyDataOmitsKey: a canonical-label
// thin row (no plugin-emitted Data) resolves to {id, kind}
// without a `data` key — workflows guarding with
// `has(entity.data)` can branch cleanly.
func TestStoreEntityResolver_EmptyDataOmitsKey(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "person:martin-wallace",
		Kind: "person",
		// No Data — thin canonical-label row.
	}))

	r := &storeEntityResolver{st: st}
	got, err := r.Resolve(ctx, "person:martin-wallace")
	require.NoError(t, err)

	assert.Equal(t, "person:martin-wallace", got["id"])
	assert.Equal(t, "person", got["kind"])
	_, hasData := got["data"]
	assert.False(t, hasData, "empty Data omits the `data` key so has() guards work")
}

// TestStoreEntityResolver_NotFoundReturnsErrEntityNotFound: the
// resolver's not-found translation feeds the workflow engine's
// missing-reference path. The decision package's
// ErrEntityNotFound sentinel triggers the note-on-task shape.
func TestStoreEntityResolver_NotFoundReturnsErrEntityNotFound(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	r := &storeEntityResolver{st: st}
	_, err = r.Resolve(context.Background(), "person:does-not-exist")
	require.Error(t, err)
	assert.True(t, errors.Is(err, decision.ErrEntityNotFound))
}

// TestStoreEntityResolver_SlugDerivedFromID pins #267: the
// canonical `<kind>:<slug>` shape per ADR-0021 surfaces the
// `slug` after-colon suffix as a top-level CEL field, so the
// documented `subject: '{{ entity.slug }}'` shape renders
// without raising `no such key: slug`. Pre-fix workflows had
// to peel via `entity.id.split(":")[1]`.
func TestStoreEntityResolver_SlugDerivedFromID(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "github-pr:acme-corp_widget_pr_42",
		Kind: "github-pr",
	}))
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "github:acme-corp_widget_pr_42",
		Kind: "github",
	}))
	// Defensive shape: id without the kind prefix degrades to
	// the full id rather than raising; pre-fix workflows that
	// depended on the bare id getting through stay functional.
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "no-colon-id",
		Kind: "github",
	}))

	r := &storeEntityResolver{st: st}

	pr, err := r.Resolve(ctx, "github-pr:acme-corp_widget_pr_42")
	require.NoError(t, err)
	assert.Equal(t, "acme-corp_widget_pr_42", pr["slug"], "canonical entity slug = after-colon suffix")

	src, err := r.Resolve(ctx, "github:acme-corp_widget_pr_42")
	require.NoError(t, err)
	assert.Equal(t, "acme-corp_widget_pr_42", src["slug"],
		"source entity slug matches the canonical slug — same suffix, different kind prefix")

	malformed, err := r.Resolve(ctx, "no-colon-id")
	require.NoError(t, err)
	assert.Equal(t, "no-colon-id", malformed["slug"], "id without `<kind>:` prefix degrades to full id")
}

// TestStoreEntityResolver_SlugRendersInSubject pins the
// docs/workflows.md canonical example (`subject:
// '{{ entity.slug }}'`) against the resolver's output —
// end-to-end check that the CEL evaluator's string template
// resolves `entity.slug` without raising. Pre-#267 this
// failed with `no such key: slug`.
func TestStoreEntityResolver_SlugRendersInSubject(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "github-pr:acme-corp_widget_pr_42",
		Kind: "github-pr",
	}))

	r := &storeEntityResolver{st: st}
	entityMap, err := r.Resolve(ctx, "github-pr:acme-corp_widget_pr_42")
	require.NoError(t, err)

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)
	prog, err := ev.Compile("entity.slug", "string")
	require.NoError(t, err)

	got, _, err := prog.EvalString(ctx, decision.Activation{Entity: entityMap})
	require.NoError(t, err, "entity.slug must resolve in a string template")
	assert.Equal(t, "acme-corp_widget_pr_42", got)
}

// TestStoreEntityResolver_CelEvalNestedAccess walks the
// end-to-end shape: build the activation, compile a CEL
// predicate reading entity.data.subject, eval against the
// resolver's output. This is the regression for #145 — the
// failure mode was `no such key: data` at CEL eval time even
// though the entity had data populated.
func TestStoreEntityResolver_CelEvalNestedAccess(t *testing.T) {
	t.Parallel()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	require.NoError(t, st.SaveEntity(ctx, &store.Entity{
		ID:   "gmail:msg-x",
		Kind: "gmail",
		Data: map[string]any{"subject": "[acme/widget] PR #42 review requested"},
	}))

	r := &storeEntityResolver{st: st}
	entityMap, err := r.Resolve(ctx, "gmail:msg-x")
	require.NoError(t, err)

	ev, err := decision.NewEvaluator(decision.Options{})
	require.NoError(t, err)
	prog, err := ev.Compile(
		`regex_capture(entity.data.subject, "\\[([^/]+/[^\\]]+)\\]", 1)`,
		"string",
	)
	require.NoError(t, err)

	got, _, err := prog.EvalString(ctx, decision.Activation{Entity: entityMap})
	require.NoError(t, err, "entity.data.<field> CEL access must succeed")
	assert.Equal(t, "acme/widget", got)
}
