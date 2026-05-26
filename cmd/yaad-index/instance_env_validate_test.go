package main

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/config"
)

// TestValidatePluginInstanceEnvReferences_AllLiterals pins the
// no-op path: a config with no `${...}` references returns nil
// + emits no warnings.
//
// t.Setenv-using tests intentionally don't call t.Parallel.
func TestValidatePluginInstanceEnvReferences_AllLiterals(t *testing.T) {
	t.Parallel()
	cfg := map[string][]config.InstanceEntry{
		"github": {
			{Name: "personal", Env: map[string]string{"YAAD_GITHUB_TOKEN": "ghp_literal_value"}},
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	err := validatePluginInstanceEnvReferences(cfg, logger)
	require.NoError(t, err)
	assert.Empty(t, buf.String(), "literal values must produce no warnings")
}

// TestValidatePluginInstanceEnvReferences_ResolvesReferences pins
// the happy path: resolved references pass validation cleanly.
func TestValidatePluginInstanceEnvReferences_ResolvesReferences(t *testing.T) {
	t.Setenv("YAAD_VALIDATE_TOKEN_OK", "resolved-secret")
	cfg := map[string][]config.InstanceEntry{
		"github": {
			{Name: "personal", Env: map[string]string{"YAAD_GITHUB_TOKEN": "${YAAD_VALIDATE_TOKEN_OK}"}},
		},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	require.NoError(t, validatePluginInstanceEnvReferences(cfg, logger))
}

// TestValidatePluginInstanceEnvReferences_MissingReferenceFailsFast
// pins the boot-time fail-fast: an unresolved `${NAME}` returns
// an error naming plugin + instance + env key so the operator
// sees the missing yaad-index.env entry at daemon boot.
func TestValidatePluginInstanceEnvReferences_MissingReferenceFailsFast(t *testing.T) {
	t.Parallel()
	cfg := map[string][]config.InstanceEntry{
		"github": {
			{Name: "personal", Env: map[string]string{"YAAD_GITHUB_TOKEN": "${YAAD_VALIDATE_NEVER_SET}"}},
		},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	err := validatePluginInstanceEnvReferences(cfg, logger)
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrUnresolvedEnvReference)
	assert.Contains(t, err.Error(), "github")
	assert.Contains(t, err.Error(), "personal")
	assert.Contains(t, err.Error(), "YAAD_GITHUB_TOKEN")
}

// TestValidatePluginInstanceEnvReferences_EmptyReferenceWarns pins
// the empty-resolution path: env var present but value is empty
// → WARN log line names the reference, validation proceeds.
func TestValidatePluginInstanceEnvReferences_EmptyReferenceWarns(t *testing.T) {
	t.Setenv("YAAD_VALIDATE_EMPTY", "")
	cfg := map[string][]config.InstanceEntry{
		"github": {
			{Name: "personal", Env: map[string]string{"YAAD_GITHUB_TOKEN": "${YAAD_VALIDATE_EMPTY}"}},
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	require.NoError(t, validatePluginInstanceEnvReferences(cfg, logger))
	out := buf.String()
	assert.Contains(t, out, "reference resolved to empty value")
	assert.Contains(t, out, "YAAD_VALIDATE_EMPTY")
	assert.Contains(t, out, "github")
	assert.Contains(t, out, "personal")
}
