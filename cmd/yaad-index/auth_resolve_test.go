package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
)

// resolveAuth precedence tests (per yaad-index a prior PR). The locked
// chain is CLI > env > config > default-true. Kong unifies CLI + env
// into a single *bool field on ServeCmd, so the test only has to
// distinguish CLI/env (one variable) from config (cfg.Auth.Required)
// from default (both unset).

func boolPtr(b bool) *bool { return &b }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func freshKeysDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(d, false))
	return d
}

func TestResolveAuth_DefaultTrue_LoadsVerifier(t *testing.T) {
	t.Parallel()
	d := freshKeysDir(t)
	cfg := &config.Config{Auth: config.AuthEntry{KeysDir: d}}

	required, verifier, err := resolveAuth(quietLogger(), nil, "", cfg)
	require.NoError(t, err)
	assert.True(t, required, "absent CLI + absent config → default true")
	assert.NotNil(t, verifier)
}

func TestResolveAuth_CLIBeatsConfig(t *testing.T) {
	t.Parallel()
	d := freshKeysDir(t)
	cfg := &config.Config{Auth: config.AuthEntry{
		KeysDir: d,
		Required: boolPtr(true), // config says true
	}}

	// CLI says false → CLI wins; verifier load skipped.
	required, verifier, err := resolveAuth(quietLogger(), boolPtr(false), "", cfg)
	require.NoError(t, err)
	assert.False(t, required)
	assert.Nil(t, verifier)
}

func TestResolveAuth_ConfigFalse_NoCLI(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Auth: config.AuthEntry{
		Required: boolPtr(false), // config opts out; CLI absent.
	}}

	required, verifier, err := resolveAuth(quietLogger(), nil, "", cfg)
	require.NoError(t, err)
	assert.False(t, required, "config.auth.required=false must take effect when CLI is absent")
	assert.Nil(t, verifier, "no verifier load when required=false")
}

func TestResolveAuth_KeysDirCLIBeatsConfig(t *testing.T) {
	t.Parallel()
	configDir := freshKeysDir(t)
	cliDir := freshKeysDir(t)
	cfg := &config.Config{Auth: config.AuthEntry{KeysDir: configDir}}

	required, verifier, err := resolveAuth(quietLogger(), boolPtr(true), cliDir, cfg)
	require.NoError(t, err)
	assert.True(t, required)
	require.NotNil(t, verifier)
	// Cross-check: signing with the CLI dir's signer must verify.
	signerCLI, err := auth.LoadSigner(cliDir)
	require.NoError(t, err)
	tok, err := signerCLI.Sign(authTestClaim())
	require.NoError(t, err)
	_, err = verifier.Verify(tok)
	assert.NoError(t, err, "verifier was loaded from --keys-dir, not auth.keys_dir")
}

func TestResolveAuth_NilConfig_DefaultTrue_BadDefaultDir(t *testing.T) {
	t.Parallel()
	// Nil config + no CLI keys-dir + default-true → tries to load
	// from /etc/yaad-index/keys/, which is unlikely to exist on a
	// test box. The error message must mention the default path so
	// operators see what to fix.
	required, verifier, err := resolveAuth(quietLogger(), nil, "", nil)
	if err != nil {
		assert.Contains(t, err.Error(), authDefaultKeysDir,
			"verifier-load error must surface the keys_dir we tried")
		assert.Nil(t, verifier)
		assert.False(t, required, "on error required is reported as false")
	} else {
		// On a box where /etc/yaad-index/keys/public.pem does happen
		// to exist (operator dev VM), the load succeeds and required
		// is true. Either path is correct; the assertion is just that
		// we got internally consistent state.
		assert.True(t, required)
		assert.NotNil(t, verifier)
	}
}

func TestResolveAuth_LoadFailure_BubblesUp(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	cfg := &config.Config{Auth: config.AuthEntry{KeysDir: missing}}

	required, verifier, err := resolveAuth(quietLogger(), boolPtr(true), "", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing,
		"verifier-load error must name the bad keys_dir so operators can act")
	assert.Nil(t, verifier)
	assert.False(t, required)
}

func authTestClaim() auth.Claim {
	now := time.Now().UTC()
	return auth.Claim{
		Subject: "bob",
		Operator: "alice",
		IssuedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
}
