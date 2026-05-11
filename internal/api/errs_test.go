package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteError_EmitsCanonicalEnvelope(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writeError(rec, http.StatusNotFound, "not_found", "no entity with id person:nope")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode response body")
	assert.False(t, body.OK)
	assert.Equal(t, "not_found", body.Error)
	assert.Equal(t, "no entity with id person:nope", body.Message)
}
