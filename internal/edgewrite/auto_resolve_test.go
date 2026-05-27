// Tests for the auto-resolver-aware CreateCanonicalEdgeByName
// path per #304 Cut C2 — caller-mode plumbing, single-match
// inline resolve, ambiguous → ResolutionDeferred, and the
// no-resolver / Interactive / already-resolved fall-through
// invariants.

package edgewrite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
)

// fakeResolver is a NameResolver fixture for the auto-mode
// tests. The map keys it on (pluginName, name) tuples; tests
// seed expected responses and assert call shape via the
// captured calls slice.
type fakeResolver struct {
	responses map[string]fakeResolverResponse
	calls     []fakeResolverCall
}

type fakeResolverResponse struct {
	entityID string
	options  map[string]plugins.DisambiguationOption
	err      error
}

type fakeResolverCall struct {
	plugin, targetKind, name string
}

func (f *fakeResolver) ResolveCanonicalEntity(_ context.Context, pluginName, targetKind, name string) (string, map[string]plugins.DisambiguationOption, error) {
	f.calls = append(f.calls, fakeResolverCall{plugin: pluginName, targetKind: targetKind, name: name})
	resp, ok := f.responses[pluginName+"|"+name]
	if !ok {
		return "", nil, errors.New("fake resolver: no response seeded for " + pluginName + "|" + name)
	}
	return resp.entityID, resp.options, resp.err
}

func newServiceWithResolver(t *testing.T, resolvers map[string][]string, resolver NameResolver) (*Service, store.Store) {
	t.Helper()
	st := newStore(t)
	svc, err := New(st, resolvers)
	require.NoError(t, err)
	svc.SetNameResolver(resolver)
	return svc, st
}

// TestModeFromContext_DefaultsInteractive pins the "callers
// that don't opt in get pre-#304 behavior" invariant.
func TestModeFromContext_DefaultsInteractive(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Interactive, ModeFromContext(context.Background()))
}

// TestModeFromContext_NilSafe pins the nil-ctx defensive path
// (linter flags passing nil context, so we use a typed-nil
// helper to exercise the branch without tripping SA1012).
func TestModeFromContext_NilSafe(t *testing.T) {
	t.Parallel()
	var ctx context.Context // nil interface value
	assert.Equal(t, Interactive, ModeFromContext(ctx))
}

func TestModeFromContext_WithMode(t *testing.T) {
	t.Parallel()
	ctx := WithMode(context.Background(), Auto)
	assert.Equal(t, Auto, ModeFromContext(ctx))
	// Parent context unaffected.
	parent := context.Background()
	assert.Equal(t, Interactive, ModeFromContext(parent))
}

// TestCreateCanonicalEdgeByName_AutoSingleMatch pins the
// happy path: auto-mode + resolver-for-kind + raw name + plugin
// returns single match → edge created with resolved id +
// resolved id returned to caller.
func TestCreateCanonicalEdgeByName_AutoSingleMatch(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{
		responses: map[string]fakeResolverResponse{
			"yaad-bgg|Brass": {entityID: "boardgame:brass-birmingham"},
		},
	}
	svc, st := newServiceWithResolver(t, map[string][]string{
		"boardgame": {"yaad-bgg"},
	}, resolver)
	seedEntity(t, st, "email:m1", "email")
	seedEntity(t, st, "boardgame:brass-birmingham", "boardgame")

	ctx := WithMode(context.Background(), Auto)
	got, created, err := svc.CreateCanonicalEdgeByName(ctx, "email:m1", "mentions", "boardgame", "Brass", nil)
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass-birmingham", got)
	assert.False(t, created, "auto-resolve branch defers entity.created to the resolver plugin's ingest path")

	require.Len(t, resolver.calls, 1)
	assert.Equal(t, "yaad-bgg", resolver.calls[0].plugin)
	assert.Equal(t, "boardgame", resolver.calls[0].targetKind, "service threads targetKind to the resolver so source-shape plugins can hop to the right canonical-kind target")
	assert.Equal(t, "Brass", resolver.calls[0].name)

	edges, err := st.GetEdgesFor(context.Background(), "email:m1", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "boardgame:brass-birmingham", edges[0].To)
}

// TestCreateCanonicalEdgeByName_AutoAmbiguous pins the
// deferred-resolution branch: auto-mode + resolver + plugin
// returns Options → ResolutionDeferred sentinel + no edge.
func TestCreateCanonicalEdgeByName_AutoAmbiguous(t *testing.T) {
	t.Parallel()
	options := map[string]plugins.DisambiguationOption{
		"boardgame:brass-birmingham": {Label: "Brass: Birmingham", Summary: "2018 Wallace"},
		"boardgame:brass-lancashire": {Label: "Brass: Lancashire", Summary: "2007 Wallace"},
	}
	resolver := &fakeResolver{
		responses: map[string]fakeResolverResponse{
			"yaad-bgg|Brass": {options: options},
		},
	}
	svc, st := newServiceWithResolver(t, map[string][]string{
		"boardgame": {"yaad-bgg"},
	}, resolver)
	seedEntity(t, st, "email:m1", "email")

	ctx := WithMode(context.Background(), Auto)
	got, created, err := svc.CreateCanonicalEdgeByName(ctx, "email:m1", "mentions", "boardgame", "Brass", nil)
	require.Error(t, err)
	assert.Equal(t, "", got)
	assert.False(t, created, "deferred-resolution must not claim a row was materialized")

	deferred, ok := IsResolutionDeferred(err)
	require.True(t, ok, "ambiguous → *ResolutionDeferred sentinel")
	assert.Equal(t, "email:m1", deferred.From)
	assert.Equal(t, "mentions", deferred.EdgeType)
	assert.Equal(t, "boardgame", deferred.TargetKind)
	assert.Equal(t, "Brass", deferred.RawTarget)
	assert.Equal(t, "yaad-bgg", deferred.ResolverPlugin)
	assert.Equal(t, options, deferred.Options)

	// No edge in the store.
	edges, err := st.GetEdgesFor(context.Background(), "email:m1", nil)
	require.NoError(t, err)
	assert.Empty(t, edges)
}

// TestCreateCanonicalEdgeByName_InteractiveBypassesResolver
// pins the "Interactive mode never invokes the resolver" rule
// even when a resolver IS registered for the kind. Falls
// through to slugify+CreateEdge — pre-#304 behavior.
func TestCreateCanonicalEdgeByName_InteractiveBypassesResolver(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{
		responses: map[string]fakeResolverResponse{}, // no responses seeded; would error if called
	}
	svc, st := newServiceWithResolver(t, map[string][]string{
		"boardgame": {"yaad-bgg"},
	}, resolver)
	seedEntity(t, st, "email:m1", "email")

	// Interactive is the default — no WithMode call.
	got, created, err := svc.CreateCanonicalEdgeByName(context.Background(), "email:m1", "mentions", "boardgame", "Brass", nil)
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass", got, "slugify locally — no plugin call")
	assert.True(t, created, "fresh slugify fall-through materializes a thin row — caller must emit entity.created")

	assert.Empty(t, resolver.calls, "Interactive mode must NOT invoke the resolver")

	edges, err := st.GetEdgesFor(context.Background(), "email:m1", nil)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "boardgame:brass", edges[0].To)
}

// TestCreateCanonicalEdgeByName_NoResolverFallThrough pins the
// operator-clarified "kind without a registered resolver →
// legacy pass-through, no task" rule. Auto mode + no resolver
// for kind = slugify + CreateEdge, no plugin call.
func TestCreateCanonicalEdgeByName_NoResolverFallThrough(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{}
	svc, st := newServiceWithResolver(t, map[string][]string{
		"boardgame": {"yaad-bgg"},
		// `person` deliberately absent from the resolver map.
	}, resolver)
	seedEntity(t, st, "email:m1", "email")

	ctx := WithMode(context.Background(), Auto)
	got, created, err := svc.CreateCanonicalEdgeByName(ctx, "email:m1", "mentions", "person", "Martin Wallace", nil)
	require.NoError(t, err)
	assert.Equal(t, "person:martin-wallace", got)
	assert.True(t, created, "no-resolver kind freshly materializes a thin row — preserves the entity.created contract")

	assert.Empty(t, resolver.calls, "no-resolver kind must NOT invoke the resolver")

	// The thin person row auto-materialized so the FK held.
	row, err := st.GetEntity(context.Background(), "person:martin-wallace")
	require.NoError(t, err)
	assert.Equal(t, "person", row.Kind)
}

// TestCreateCanonicalEdgeByName_AlreadyResolvedSkipsResolver
// pins the "target already a `<kind>:<slug>` id" branch — the
// service detects the canonical-id prefix + falls through
// to legacy CreateEdge instead of treating it as a raw name.
func TestCreateCanonicalEdgeByName_AlreadyResolvedSkipsResolver(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{}
	svc, st := newServiceWithResolver(t, map[string][]string{
		"boardgame": {"yaad-bgg"},
	}, resolver)
	seedEntity(t, st, "email:m1", "email")

	ctx := WithMode(context.Background(), Auto)
	got, created, err := svc.CreateCanonicalEdgeByName(ctx, "email:m1", "mentions", "boardgame", "boardgame:caverna", nil)
	require.NoError(t, err)
	assert.Equal(t, "boardgame:caverna", got, "canonical-ID-shape name strips the prefix and reuses the slug")
	assert.True(t, created, "first-touch of boardgame:caverna materializes the thin row")

	assert.Empty(t, resolver.calls, "already-resolved name must NOT invoke the resolver")
}

// TestCreateCanonicalEdgeByName_NilResolverFallThrough pins
// that an unwired resolver is the same as no-resolver-for-kind:
// auto-mode + resolver-map-claims-the-kind, but the resolver
// itself is nil → fall through to slugify (don't panic, don't
// silently misroute).
func TestCreateCanonicalEdgeByName_NilResolverFallThrough(t *testing.T) {
	t.Parallel()
	svc, st := newServiceWithResolver(t, map[string][]string{
		"boardgame": {"yaad-bgg"},
	}, nil) // nil resolver wired explicitly
	seedEntity(t, st, "email:m1", "email")

	ctx := WithMode(context.Background(), Auto)
	got, created, err := svc.CreateCanonicalEdgeByName(ctx, "email:m1", "mentions", "boardgame", "Brass", nil)
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass", got, "nil resolver → fall through to slugify")
	assert.True(t, created, "nil-resolver fall-through still materializes the thin row")
}

// TestCreateCanonicalEdgeByName_ResolverError surfaces
// transport failures back to the caller without creating an
// edge or defer sentinel.
func TestCreateCanonicalEdgeByName_ResolverError(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{
		responses: map[string]fakeResolverResponse{
			"yaad-bgg|Brass": {err: errors.New("plugin offline")},
		},
	}
	svc, st := newServiceWithResolver(t, map[string][]string{
		"boardgame": {"yaad-bgg"},
	}, resolver)
	seedEntity(t, st, "email:m1", "email")

	ctx := WithMode(context.Background(), Auto)
	_, _, err := svc.CreateCanonicalEdgeByName(ctx, "email:m1", "mentions", "boardgame", "Brass", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin offline")
	// Not a ResolutionDeferred — the sentinel reserved for
	// the ambiguous-options shape, not transport failures.
	_, deferred := IsResolutionDeferred(err)
	assert.False(t, deferred)

	edges, _ := st.GetEdgesFor(context.Background(), "email:m1", nil)
	assert.Empty(t, edges)
}

// TestCreateCanonicalEdgeByName_RejectsEmptyArgs pins the
// strict-boundary input validation.
func TestCreateCanonicalEdgeByName_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceWithResolver(t, nil, nil)
	cases := []struct {
		name, from, edge, kind, target string
	}{
		{"empty from", "", "mentions", "boardgame", "Brass"},
		{"empty edgeType", "email:m1", "", "boardgame", "Brass"},
		{"empty targetKind", "email:m1", "mentions", "", "Brass"},
		{"whitespace targetName", "email:m1", "mentions", "boardgame", "   "},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := svc.CreateCanonicalEdgeByName(context.Background(), c.from, c.edge, c.kind, c.target, nil)
			require.Error(t, err)
		})
	}
}

// TestResolutionDeferred_ErrorsAsExtractsStructured pins the
// errors.As pathway Cut C3 will rely on to pull options + tuple
// off the sentinel.
func TestResolutionDeferred_ErrorsAsExtractsStructured(t *testing.T) {
	t.Parallel()
	d := &ResolutionDeferred{
		From: "email:m1", EdgeType: "mentions", TargetKind: "boardgame",
		RawTarget: "Brass", ResolverPlugin: "yaad-bgg",
		Options: map[string]plugins.DisambiguationOption{
			"boardgame:brass-birmingham": {Label: "Brass: Birmingham"},
		},
	}
	var wrapped error = d
	got, ok := IsResolutionDeferred(wrapped)
	require.True(t, ok)
	assert.Equal(t, "email:m1", got.From)
	assert.Equal(t, "Brass", got.RawTarget)
	assert.Contains(t, d.Error(), "Brass")
}

// timeBoundary keeps the test honest about how the helper
// behaves under a deadline-exceeded ctx. The auto-resolver
// path goes through the user-supplied ctx so a cancelled ctx
// bubbles via the resolver's err return.
func TestCreateCanonicalEdgeByName_CtxCancelPropagates(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := &fakeResolver{
		responses: map[string]fakeResolverResponse{
			"yaad-bgg|Brass": {err: context.Canceled},
		},
	}
	svc, _ := newServiceWithResolver(t, map[string][]string{"boardgame": {"yaad-bgg"}}, resolver)
	ctx = WithMode(ctx, Auto)
	_, _, err := svc.CreateCanonicalEdgeByName(ctx, "email:m1", "mentions", "boardgame", "Brass", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	_ = time.Second // keep the time import alive for parallel-test sanity
}
