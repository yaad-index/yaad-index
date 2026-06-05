package mcp

import (
	"context"
	"net/url"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func invokeListEntities(t *testing.T, args map[string]any) *queryCaptureHandler {
	t.Helper()
	h := &queryCaptureHandler{}
	b := newBridge(h)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := listEntitiesHandler(b)(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "list_entities should not error: %+v", res)
	return h
}

// TestListEntities_TagsFilter pins #453: each tag in the `tags` array
// threads onto the GET /v1/search query as a repeated `tags=` param,
// url-escaped, alongside the required kind.
func TestListEntities_TagsFilter(t *testing.T) {
	t.Parallel()
	h := invokeListEntities(t, map[string]any{
		"kind": "boardgame",
		"tags": []any{"alpha", "beta"},
	})
	assert.Equal(t, "/v1/search", h.path)
	vals, err := url.ParseQuery(h.query)
	require.NoError(t, err)
	assert.Equal(t, "boardgame", vals.Get("kind"))
	assert.Equal(t, []string{"alpha", "beta"}, vals["tags"])
}

// TestListEntities_TagsEscaped pins that tag values are url-escaped on
// the synthesized query.
func TestListEntities_TagsEscaped(t *testing.T) {
	t.Parallel()
	h := invokeListEntities(t, map[string]any{
		"kind": "boardgame",
		"tags": []any{"two words"},
	})
	vals, err := url.ParseQuery(h.query)
	require.NoError(t, err)
	assert.Equal(t, []string{"two words"}, vals["tags"], "round-trips through escaping")
}

// TestListEntities_NoTagsOmitsParam pins the preserved default: an absent
// or empty tags array adds no `tags=` param (full kind listing).
func TestListEntities_NoTagsOmitsParam(t *testing.T) {
	t.Parallel()
	h := invokeListEntities(t, map[string]any{"kind": "boardgame"})
	vals, _ := url.ParseQuery(h.query)
	_, has := vals["tags"]
	assert.False(t, has, "no tags param when none supplied")
	assert.Equal(t, "boardgame", vals.Get("kind"))
}

// TestListEntities_TagsAndJournalCompose pins that the tags filter
// composes with is_journal on the same query.
func TestListEntities_TagsAndJournalCompose(t *testing.T) {
	t.Parallel()
	h := invokeListEntities(t, map[string]any{
		"kind":       "day",
		"is_journal": true,
		"tags":       []any{"alpha"},
	})
	vals, err := url.ParseQuery(h.query)
	require.NoError(t, err)
	assert.Equal(t, "day", vals.Get("kind"))
	assert.Equal(t, "true", vals.Get("is_journal"))
	assert.Equal(t, []string{"alpha"}, vals["tags"])
}
