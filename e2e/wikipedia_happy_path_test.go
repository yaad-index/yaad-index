//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_WikipediaHappyPath ingests a Wikipedia URL through the
// daemon, with a mock upstream, and asserts source entity + vault
// file + /v1/entities round-trip.
func TestE2E_WikipediaHappyPath(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/rest_v1/page/summary/"):
			_, _ = fmt.Fprint(w, `{"title":"Go (programming language)","lang":"en"}`)
		case r.URL.Path == "/w/api.php":
			// Action API — return the full plaintext extract the
			// plugin reads as RawContent for the body emit.
			_, _ = fmt.Fprint(w, `{"query":{"pages":[{"pageid":1,"title":"Go (programming language)","extract":"Go is a statically typed, compiled programming language designed at Google."}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)

	h := Start(t, HarnessConfig{
		Plugins: []PluginConfig{
			{
				Name: "yaad-wikipedia",
				Env: map[string]string{
					"YAAD_WIKIPEDIA_API_HOST_OVERRIDE": mock.URL,
				},
			},
		},
	})

	status, body := h.PostJSON("/v1/ingest", map[string]any{
		"url":          "https://en.wikipedia.org/wiki/Go_(programming_language)",
		"wait_seconds": 30,
	})
	// 202/needs_fill: yaad-wikipedia emits universal summary+tags
	// gap names; the daemon's needs_fill path fires.
	require.Equal(t, http.StatusAccepted, status, "ingest body=%s", body)

	var ingestResp struct {
		OK     bool   `json:"ok"`
		State  string `json:"state"`
		Entity struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"entity"`
	}
	require.NoError(t, json.Unmarshal(body, &ingestResp))
	assert.True(t, ingestResp.OK)
	assert.Equal(t, "needs_fill", ingestResp.State)
	require.NotEmpty(t, ingestResp.Entity.ID)
	assert.Equal(t, "wikipedia-article", ingestResp.Entity.Kind)

	// Vault file lands at <vault>/<kind>/<slug>.md.
	slug := strings.TrimPrefix(ingestResp.Entity.ID, ingestResp.Entity.Kind+":")
	expectVaultFile := filepath.Join(h.VaultPath, ingestResp.Entity.Kind, slug+".md")
	_, err := os.Stat(expectVaultFile)
	require.NoError(t, err, "expected vault file at %s", expectVaultFile)

	// GET /v1/entities/{id} returns the entity shape directly.
	status, body = h.GetJSON("/v1/entities/" + ingestResp.Entity.ID)
	require.Equal(t, http.StatusOK, status, "entity GET body=%s", body)
	var entity struct {
		ID     string         `json:"id"`
		Kind   string         `json:"kind"`
		Source []string       `json:"source"`
		Data   map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &entity))
	assert.Equal(t, ingestResp.Entity.ID, entity.ID)
	assert.Equal(t, "wikipedia-article", entity.Kind)
	// Per ADR-0028 §5, the wire shape is now the slash-form
	// source array. Single-implicit-instance plugins like
	// yaad-wikipedia surface `<plugin>/default` (Cut 1's Load
	// synthesizes the default instance when no `instances:`
	// block is declared in operator config).
	assert.Equal(t, []string{"yaad-wikipedia/default"}, entity.Source)
}
