package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfig is a tiny helper for the table-driven tests below.
// Returns the path to the freshly-written file inside a t.TempDir.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644), "write config")
	return path
}

// makeExecutable writes an executable stub at <dir>/<name> and returns
// its absolute path. Used by the "happy path" test where the config
// references a real file with the executable bit set.
func makeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755), "write executable")
	return p
}

func TestLoad_HappyPath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	wikipediaPath := makeExecutable(t, tmp, "yaad-wikipedia")
	bggPath := makeExecutable(t, tmp, "yaad-bgg")

	cfgPath := writeConfig(t, `
plugins:
  - name: wikipedia
    path: `+wikipediaPath+`
  - name: bgg
    path: `+bggPath+`
`)

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.Len(t, cfg.Plugins, 2)
	// Order must match the YAML source — first-match-wins dispatch
	// priority depends on this. Don't refactor into a name→path map
	// lookup; that would silently mask order regressions.
	assert.Equal(t, "wikipedia", cfg.Plugins[0].Name)
	assert.Equal(t, wikipediaPath, cfg.Plugins[0].Path)
	assert.Equal(t, "bgg", cfg.Plugins[1].Name)
	assert.Equal(t, bggPath, cfg.Plugins[1].Path)
}

func TestLoad_PreservesYAMLOrder(t *testing.T) {
	t.Parallel()

	// Regression guard for the slice-vs-map fix (the cold-reviewer's PR #28
	// finding #4): if Plugins ever reverts to a map, Go's randomized
	// map iteration would scramble first-match-wins dispatch priority
	// across server restarts. The assertion below is intentionally
	// strict on positional order, not just set membership.
	tmp := t.TempDir()
	a := makeExecutable(t, tmp, "a")
	b := makeExecutable(t, tmp, "b")
	c := makeExecutable(t, tmp, "c")

	cfgPath := writeConfig(t, `
plugins:
  - name: c
    path: `+c+`
  - name: a
    path: `+a+`
  - name: b
    path: `+b+`
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	got := []string{cfg.Plugins[0].Name, cfg.Plugins[1].Name, cfg.Plugins[2].Name}
	assert.Equal(t, []string{"c", "a", "b"}, got)
}

func TestLoad_RejectsDuplicateNames(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	bin := makeExecutable(t, tmp, "yaad-thing")

	cfgPath := writeConfig(t, `
plugins:
  - name: wikipedia
    path: `+bin+`
  - name: wikipedia
    path: `+bin+`
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestLoad_MissingFileReturnsErrFileMissing(t *testing.T) {
	t.Parallel()

	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	assert.ErrorIs(t, err, ErrFileMissing)
}

func TestLoad_RejectsRelativePaths(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins:
  - name: wikipedia
    path: ./yaad-wikipedia
`)
	_, err := Load(cfgPath)
	require.Error(t, err, "Load with relative path")
	assert.Contains(t, err.Error(), "not absolute")
}

func TestLoad_RejectsBareNamePath(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins:
  - name: wikipedia
    path: yaad-wikipedia
`)
	_, err := Load(cfgPath)
	assert.Error(t, err, "Load with bare name (no PATH search per ADR-0006)")
}

func TestLoad_RejectsTildePathsAsNotAbsolute(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins:
  - name: wikipedia
    path: ~/.local/bin/yaad-wikipedia
`)
	_, err := Load(cfgPath)
	assert.Error(t, err, "Load with `~/` path (no shell expansion per ADR-0006)")
}

func TestLoad_RejectsMissingBinary(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins:
  - name: wikipedia
    path: /this/path/definitely/does/not/exist/yaad-wikipedia
`)
	_, err := Load(cfgPath)
	assert.Error(t, err, "Load with missing binary")
}

func TestLoad_RejectsNonExecutable(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	notExec := filepath.Join(tmp, "not-executable")
	require.NoError(t, os.WriteFile(notExec, []byte("not exec"), 0o644), "write file")

	cfgPath := writeConfig(t, `
plugins:
  - name: wikipedia
    path: `+notExec+`
`)
	_, err := Load(cfgPath)
	require.Error(t, err, "Load with non-executable")
	assert.Contains(t, err.Error(), "not executable")
}

func TestLoad_RejectsDirectoryAsPath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfgPath := writeConfig(t, `
plugins:
  - name: wikipedia
    path: `+tmp+`
`)
	_, err := Load(cfgPath)
	require.Error(t, err, "Load with directory path")
	assert.Contains(t, err.Error(), "directory")
}

func TestLoad_EmptyPluginsListIsValid(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `plugins: []`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err, "empty plugins list")
	assert.Empty(t, cfg.Plugins)
}

func TestLoad_AbsentPluginsKeyIsValid(t *testing.T) {
	t.Parallel()

	// Empty file (or one with no plugins: key) means "no plugins" —
	// distinct from "missing file" but functionally the same as far
	// as the registry is concerned.
	cfgPath := writeConfig(t, ``)
	cfg, err := Load(cfgPath)
	require.NoError(t, err, "empty file")
	assert.Empty(t, cfg.Plugins)
}

func TestLoad_MalformedYAMLReturnsError(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins:
  wikipedia: [this, is, not, a, string
`)
	_, err := Load(cfgPath)
	assert.Error(t, err, "malformed YAML")
}

func TestLoad_VaultPathParsedWhenAbsent(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `plugins: []`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Empty(t, cfg.Vault.Path, "absent vault key → empty Path")
}

func TestLoad_VaultPathHappyPath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: `+tmp+`
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, tmp, cfg.Vault.Path)
}

func TestLoad_VaultPathRejectsRelative(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: relative/vault
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestLoad_VaultPathRejectsMissing(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: /this/does/not/exist/test-vault
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
}

func TestLoad_VaultPathRejectsFileAsRoot(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	notDir := filepath.Join(tmp, "file")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o644))

	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: `+notDir+`
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestLoad_CanonicalKindsParsed(t *testing.T) {
	t.Parallel()

	// Migrated to ADR-0013 §1's map shape (yaad-index #163 PR-2):
	// each enabled kind owns a per-kind config block carrying its
	// gap-set vocabulary + optional fill instruction. Map keys are
	// the enabled-kinds set.
	cfgPath := writeConfig(t, `
plugins: []
canonical_kinds:
  person:
    gaps:
      name: "Full name."
      summary: "One-paragraph summary."
    instruction: "Skip if absent."
  city:
    gaps:
      name: "City name."
canonical_edge_types:
  - is_about
  - lives_in
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.Len(t, cfg.CanonicalKinds, 2)

	// Per ADR-0016: Gaps are typed (GapSpec{Type, Description});
	// shorthand `gaps: {name: "..."}` decodes via the custom
	// UnmarshalYAML to {Type: "string", Description: "..."}.
	// Instruction is *InstructionSpec; bare-string shorthand
	// decodes to {Enabled: true, Text: "..."}.
	person, ok := cfg.CanonicalKinds["person"]
	require.True(t, ok, "person kind in registry")
	assert.Equal(t, "Full name.", person.Gaps["name"].Description)
	assert.Equal(t, "string", person.Gaps["name"].Type, "shorthand defaults Type to string")
	assert.Equal(t, "One-paragraph summary.", person.Gaps["summary"].Description)
	require.NotNil(t, person.Instruction, "instruction shorthand parses as enabled struct")
	assert.True(t, person.Instruction.Enabled, "shorthand instruction → Enabled=true")
	assert.Equal(t, "Skip if absent.", person.Instruction.Text)

	city, ok := cfg.CanonicalKinds["city"]
	require.True(t, ok, "city kind in registry")
	assert.Equal(t, "City name.", city.Gaps["name"].Description)
	assert.Nil(t, city.Instruction, "instruction omitted at the per-kind layer → nil")

	assert.Equal(t, []string{"is_about", "lives_in"}, cfg.CanonicalEdgeTypes)
}

func TestLoad_CanonicalKindsAbsent(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `plugins: []`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Empty(t, cfg.CanonicalKinds, "absent canonical_kinds → empty registry")
	assert.Empty(t, cfg.CanonicalEdgeTypes)
}

func TestLoad_CanonicalKindsExplicitlyEmpty(t *testing.T) {
	t.Parallel()

	// Locks the nil-vs-empty observational-equivalence property: an
	// explicitly empty `canonical_kinds: {}` produces the same loaded
	// state as the key being absent. Both → no canonical layer.
	cfgPath := writeConfig(t, `
plugins: []
canonical_kinds: {}
canonical_edge_types: []
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Empty(t, cfg.CanonicalKinds)
	assert.Empty(t, cfg.CanonicalEdgeTypes)

	// Guard built from the loaded map (via key extraction) behaves
	// identically whether the registry was nil or empty.
	g := NewCanonicalGuard(canonicalKindNamesForTest(cfg.CanonicalKinds), cfg.CanonicalEdgeTypes)
	assert.False(t, g.AllowKind("person"))
	assert.False(t, g.AllowEdgeType("is_about"))
}

// canonicalKindNamesForTest matches cmd/yaad-index/main.go's helper
// behavior — including the nil-return on empty/nil input — so guard
// construction in this test exercises the same code path the
// production wiring uses. Tests don't share package boundaries
// with cmd, so duplicating the small helper beats inventing a
// public surface for a one-call use.
func canonicalKindNamesForTest(reg map[string]CanonicalKindConfig) []string {
	if len(reg) == 0 {
		return nil
	}
	out := make([]string, 0, len(reg))
	for k := range reg {
		out = append(out, k)
	}
	return out
}

// Validation: kind name must match [a-z][a-z0-9_]*.
func TestLoad_CanonicalKinds_RejectsInvalidKindName(t *testing.T) {
	t.Parallel()
	for _, badName := range []string{"Person", "1city", "person-foo", "person foo"} {
		badName := badName
		t.Run(badName, func(t *testing.T) {
			t.Parallel()
			cfgPath := writeConfig(t, fmt.Sprintf(`
plugins: []
canonical_kinds:
  %q:
    gaps:
      name: "x"
`, badName))
			_, err := Load(cfgPath)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "canonical_kinds."+badName)
			assert.Contains(t, err.Error(), "invalid kind name")
		})
	}
}

// Per ADR-0016 §3: a per-kind block can omit `gaps:` entirely
// because the merged effective registry inherits name/tags/summary
// from code defaults plus any plugin-declared extras. The pre-
// ADR-0016 "must declare at least one gap" rule was a foot-gun
// (every kind ended up with the same name/tags/summary boilerplate)
// — empty per-kind gaps now means "rely on layered defaults".
func TestLoad_CanonicalKinds_AcceptsEmptyGaps(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
canonical_kinds:
  person:
    gaps: {}
    instruction: "x"
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err, "empty gaps block is valid post-ADR-0016 (layered defaults supply name/tags/summary)")
	person, ok := cfg.CanonicalKinds["person"]
	require.True(t, ok)
	assert.Empty(t, person.Gaps, "operator's per-kind gaps explicitly empty")
	require.NotNil(t, person.Instruction)
	assert.Equal(t, "x", person.Instruction.Text)
}

// Validation: gap field name regex.
func TestLoad_CanonicalKinds_RejectsInvalidGapFieldName(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
canonical_kinds:
  person:
    gaps:
      "Bad-Name": "prompt"
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical_kinds.person.gaps.Bad-Name")
	assert.Contains(t, err.Error(), "invalid gap field name")
}

// Validation: empty gap description rejected. Per ADR-0016 the
// internal field name became `description` (typed shape) and the
// validator's error scope follows.
func TestLoad_CanonicalKinds_RejectsEmptyGapPrompt(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
canonical_kinds:
  person:
    gaps:
      name: ""
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical_kinds.person.gaps.name")
	assert.Contains(t, err.Error(), "description: cannot be empty")
}

// Validation: whitespace-only gap prompt is a false-signal forwarded
// verbatim to the AI — same problem as a whitespace-only instruction.
// Mirrors TestLoad_CanonicalKinds_RejectsWhitespaceInstruction (the cold-reviewer's
// PR-164 catch).
func TestLoad_CanonicalKinds_RejectsWhitespaceGapPrompt(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
canonical_kinds:
  person:
    gaps:
      name: "   \t  \n  "
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical_kinds.person.gaps.name")
	assert.Contains(t, err.Error(), "whitespace-only")
}

// Validation: whitespace-only instruction is a false signal.
func TestLoad_CanonicalKinds_RejectsWhitespaceInstruction(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
canonical_kinds:
  person:
    gaps:
      name: "x"
    instruction: "   \t  \n  "
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical_kinds.person.instruction")
	assert.Contains(t, err.Error(), "whitespace-only")
}

// Per-kind instruction + global fill_instruction co-exist at the
// config layer; their interaction (per-kind override semantics) is
// PR-3's concern. PR-2 just verifies they parse together.
func TestLoad_CanonicalKinds_CoexistsWithFillInstruction(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
fill_instruction: "Skip absent gaps."
canonical_kinds:
  person:
    gaps:
      name: "Full name."
    instruction: "Override for person."
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "Skip absent gaps.", cfg.FillInstruction)
	require.NotNil(t, cfg.CanonicalKinds["person"].Instruction)
	assert.Equal(t, "Override for person.", cfg.CanonicalKinds["person"].Instruction.Text)
	assert.True(t, cfg.CanonicalKinds["person"].Instruction.Enabled, "shorthand instruction → Enabled=true")
}

func TestLoad_LogLevelParsed(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins: []
log_level: warn
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "warn", cfg.LogLevel)
}

func TestLoad_LogLevelDefaultsToEmpty(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `plugins: []`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Empty(t, cfg.LogLevel,
		"absent log_level → empty string; ParseLogLevel maps that to info")
}

// Per yaad-index #195: operator-configured timezone parses via
// time.LoadLocation at Validate time. Empty defaults to UTC.
func TestLoad_TimezoneParsedFromConfig(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
timezone: America/Los_Angeles
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "America/Los_Angeles", cfg.Timezone)
}

func TestLoad_TimezoneDefaultsToEmpty(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `plugins: []`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Empty(t, cfg.Timezone,
		"absent timezone → empty string; main.go reads as UTC default")
}

func TestLoad_TimezoneRejectsBadIANA(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
timezone: Not_A_Real_Zone
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timezone")
	assert.Contains(t, err.Error(), "Not_A_Real_Zone",
		"err should name the bad identifier so operators can grep")
}

func TestLoad_LogLevelRejectsUnknown(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins: []
log_level: trace
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log_level")
	assert.Contains(t, err.Error(), "trace",
		"err should name the bad value so operators can grep")
}

func TestLoad_VaultAutoCommitTrueRequiresGitDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: `+tmp+`
  auto_commit: true
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".git")
}

func TestLoad_VaultAutoCommitTrueAcceptsWhenGitDirExists(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	require.NoError(t, os.Mkdir(tmp+"/.git", 0o755))
	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: `+tmp+`
  auto_commit: true
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Vault.AutoCommit)
	assert.True(t, *cfg.Vault.AutoCommit)
}

func TestLoad_VaultAutoCommitFalseDoesNotRequireGitDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: `+tmp+`
  auto_commit: false
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Vault.AutoCommit)
	assert.False(t, *cfg.Vault.AutoCommit)
}

func TestLoad_VaultAutoCommitNilAutoDetects(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: `+tmp+`
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Nil(t, cfg.Vault.AutoCommit, "absent auto_commit → nil → auto-detect at startup")
}

func TestLoad_VaultAutoCommitDebounceRejectsNegative(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: `+tmp+`
  auto_commit_debounce_seconds: -3
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto_commit_debounce_seconds")
}

func TestLoad_VaultAutoPushRequiresAutoCommit(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfgPath := writeConfig(t, `
plugins: []
vault:
  path: `+tmp+`
  auto_commit: false
  auto_push: true
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto_push")
	assert.Contains(t, err.Error(), "auto_commit")
}

func TestLoad_VaultAutoCommitFieldsRequireVaultPath(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfig(t, `
plugins: []
vault:
  auto_commit: true
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vault.path")
}

func TestLoad_AuthFieldsParse(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
auth:
  keys_dir: /etc/yaad-index/keys
  default_ttl: 720h
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "/etc/yaad-index/keys", cfg.Auth.KeysDir)
	assert.Equal(t, "720h", cfg.Auth.DefaultTTL)
}

func TestLoad_AuthAbsent_LeavesEmptyEntry(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `plugins: []`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Empty(t, cfg.Auth.KeysDir)
	assert.Empty(t, cfg.Auth.DefaultTTL)
	assert.Nil(t, cfg.Auth.Required,
		"auth.required must remain nil (tri-state) when absent so the resolution chain falls through to the default")
}

// TestLoad_AuthRequired_TriState pins the *bool tri-state shape on
// auth.required: absent → nil (CLI/env/default takes over), explicit
// `true` → *true (operator opts in), explicit `false` → *false
// (operator opts out for dev mode). The precedence chain in
// cmd/yaad-index/main.go relies on the absent-vs-explicit-false
// distinction; collapsing this to a plain bool would silently break it.
func TestLoad_AuthRequired_TriState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		want *bool
	}{
		{"explicit_true", "auth:\n  required: true\n", boolPtr(true)},
		{"explicit_false", "auth:\n  required: false\n", boolPtr(false)},
		{"absent", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := "plugins: []\n" + tc.yaml
			cfgPath := writeConfig(t, body)
			cfg, err := Load(cfgPath)
			require.NoError(t, err)
			if tc.want == nil {
				assert.Nil(t, cfg.Auth.Required)
			} else {
				require.NotNil(t, cfg.Auth.Required)
				assert.Equal(t, *tc.want, *cfg.Auth.Required)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// TestLoad_UserContentFrontmatterEdges_HappyPath pins the parse +
// validation of the per-#238 operator-side mapping field. Mirrors
// the plugin-side `frontmatter_edges:` Capabilities shape.
func TestLoad_UserContentFrontmatterEdges_HappyPath(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
user_content_frontmatter_edges:
  about:
    edge_type: is_about
    target_kind: boardgame
  mentions:
    edge_type: mentions
    target_kind: person
  designed_by:
    edge_type: designed_by
    target_kind: person
`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.Len(t, cfg.UserContentFrontmatterEdges, 3)
	assert.Equal(t, "is_about", cfg.UserContentFrontmatterEdges["about"].EdgeType)
	assert.Equal(t, "boardgame", cfg.UserContentFrontmatterEdges["about"].TargetKind)
	assert.Equal(t, "mentions", cfg.UserContentFrontmatterEdges["mentions"].EdgeType)
	assert.Equal(t, "person", cfg.UserContentFrontmatterEdges["mentions"].TargetKind)
	assert.Equal(t, "designed_by", cfg.UserContentFrontmatterEdges["designed_by"].EdgeType)
}

// TestLoad_UserContentFrontmatterEdges_AbsentIsNil pins the
// absent-section-is-zero-value contract: pre-#238 configs (no
// `user_content_frontmatter_edges:` block) must continue to parse
// cleanly, with the field nil so the UGC create/edit handlers'
// derivation step is a no-op.
func TestLoad_UserContentFrontmatterEdges_AbsentIsNil(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `plugins: []`)
	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Nil(t, cfg.UserContentFrontmatterEdges,
		"absent block must leave the map nil — gates the derivation no-op")
}

// TestLoad_UserContentFrontmatterEdges_EmptyEdgeType rejects a
// mapping whose edge_type is whitespace-only — likely a typo and
// silently dropping every edge of that field would be surprising.
func TestLoad_UserContentFrontmatterEdges_EmptyEdgeType(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
user_content_frontmatter_edges:
  mentions:
    edge_type: ""
    target_kind: person
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "edge_type is required")
}

// TestLoad_UserContentFrontmatterEdges_EmptyTargetKind rejects a
// mapping with no target_kind — the slugify-and-stub step needs
// the kind to construct `<kind>:<slug>` IDs.
func TestLoad_UserContentFrontmatterEdges_EmptyTargetKind(t *testing.T) {
	t.Parallel()
	cfgPath := writeConfig(t, `
plugins: []
user_content_frontmatter_edges:
  about:
    edge_type: is_about
    target_kind: ""
`)
	_, err := Load(cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target_kind is required")
}
