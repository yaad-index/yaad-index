// Tests for the #48 slice 4 boot-time canonical-registry audit
// logging. The audit must surface kinds with non-trivial merge
// at INFO and stay silent on the vanilla-install case.

package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBufferLogger returns a JSON-handler slog logger backed by
// a byte buffer caller can assert against. Level threaded
// through so DEBUG-only behavior can be tested independently.
func newBufferLogger(t *testing.T, level slog.Level) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
	return logger, &buf
}

// TestBootAudit_SilentOnVanillaInstall pins the signal-only
// gate: a merged registry with only code_defaults +
// builtin_kind provenance (no plugin extras, no operator
// overrides) produces NO INFO lines.
func TestBootAudit_SilentOnVanillaInstall(t *testing.T) {
	t.Parallel()
	reg := map[string]CanonicalKindConfig{
		"boardgame": {Gaps: map[string]GapSpec{"name": {Description: "n"}}},
	}
	prov := RegistryProvenance{
		"boardgame": {
			"name":                  LayerUniversalDefaults,
			"rating":                LayerBuiltinKindGaps,
			"owned":                 LayerBuiltinKindGaps,
			InstructionProvenanceKey: LayerUniversalDefaults,
		},
	}
	logger, buf := newBufferLogger(t, slog.LevelInfo)
	LogCanonicalRegistryBootAudit(logger, reg, prov)

	out := buf.String()
	assert.NotContains(t, out, `"msg":"canonical-kind merge"`,
		"vanilla install with only code/builtin layers MUST stay silent at INFO; got %s", out)
}

// TestBootAudit_LogsKindWithPluginExtras pins that plugin
// contributions trigger the INFO line. Counts surface in the
// structured attributes.
func TestBootAudit_LogsKindWithPluginExtras(t *testing.T) {
	t.Parallel()
	reg := map[string]CanonicalKindConfig{
		"boardgame": {Gaps: map[string]GapSpec{}},
	}
	prov := RegistryProvenance{
		"boardgame": {
			"name":                  LayerUniversalDefaults,
			"rating":                LayerBuiltinKindGaps,
			"bgg_id":                LayerPluginExtras,
			"thumb":                 LayerPluginExtras,
			InstructionProvenanceKey: LayerUniversalDefaults,
		},
	}
	logger, buf := newBufferLogger(t, slog.LevelInfo)
	LogCanonicalRegistryBootAudit(logger, reg, prov)

	out := buf.String()
	assert.Contains(t, out, `"msg":"canonical-kind merge"`)
	assert.Contains(t, out, `"kind":"boardgame"`)
	assert.Contains(t, out, `"plugin_extras":2`)
	assert.Contains(t, out, `"operator":0`)
	assert.Contains(t, out, `"instruction_layer":"code_defaults"`)
}

// TestBootAudit_LogsKindWithOperatorOverride pins that an
// operator-per-kind override (e.g. operator rewrote
// `rating.description`) triggers the INFO line.
func TestBootAudit_LogsKindWithOperatorOverride(t *testing.T) {
	t.Parallel()
	reg := map[string]CanonicalKindConfig{
		"book": {Gaps: map[string]GapSpec{}},
	}
	prov := RegistryProvenance{
		"book": {
			"name":                  LayerUniversalDefaults,
			"author":                LayerBuiltinKindGaps,
			"rating":                LayerOperatorPerKind,
			InstructionProvenanceKey: LayerOperatorPerKind,
		},
	}
	logger, buf := newBufferLogger(t, slog.LevelInfo)
	LogCanonicalRegistryBootAudit(logger, reg, prov)

	out := buf.String()
	assert.Contains(t, out, `"kind":"book"`)
	assert.Contains(t, out, `"operator":1`)
	assert.Contains(t, out, `"instruction_layer":"operator"`)
}

// TestBootAudit_LogsInLexOrder pins that multiple interesting
// kinds emit INFO lines in lex order so logs read deterministically
// across runs.
func TestBootAudit_LogsInLexOrder(t *testing.T) {
	t.Parallel()
	reg := map[string]CanonicalKindConfig{
		"zoo":      {Gaps: map[string]GapSpec{}},
		"alpha":    {Gaps: map[string]GapSpec{}},
		"mid":      {Gaps: map[string]GapSpec{}},
	}
	prov := RegistryProvenance{
		"zoo":   {"x": LayerOperatorPerKind, InstructionProvenanceKey: LayerUniversalDefaults},
		"alpha": {"x": LayerPluginExtras, InstructionProvenanceKey: LayerUniversalDefaults},
		"mid":   {"x": LayerOperatorDefaults, InstructionProvenanceKey: LayerUniversalDefaults},
	}
	logger, buf := newBufferLogger(t, slog.LevelInfo)
	LogCanonicalRegistryBootAudit(logger, reg, prov)

	out := buf.String()
	alphaIdx := strings.Index(out, `"kind":"alpha"`)
	midIdx := strings.Index(out, `"kind":"mid"`)
	zooIdx := strings.Index(out, `"kind":"zoo"`)
	require.True(t, alphaIdx >= 0 && midIdx >= 0 && zooIdx >= 0, "all three kinds must log; got %s", out)
	assert.Less(t, alphaIdx, midIdx, "alpha must log before mid")
	assert.Less(t, midIdx, zooIdx, "mid must log before zoo")
}

// TestBootAudit_DebugDumpsFullProvenance pins that at DEBUG
// level the full per-(kind, field) provenance lands on a
// structured attribute (operators piping through jq can read
// every tuple).
func TestBootAudit_DebugDumpsFullProvenance(t *testing.T) {
	t.Parallel()
	reg := map[string]CanonicalKindConfig{
		"book": {Gaps: map[string]GapSpec{}},
	}
	prov := RegistryProvenance{
		"book": {
			"name":                  LayerUniversalDefaults,
			"author":                LayerBuiltinKindGaps,
			InstructionProvenanceKey: LayerUniversalDefaults,
		},
	}
	logger, buf := newBufferLogger(t, slog.LevelDebug)
	LogCanonicalRegistryBootAudit(logger, reg, prov)

	out := buf.String()
	assert.Contains(t, out, `"msg":"canonical-kind merge audit (full registry)"`)
	assert.Contains(t, out, "book.author=builtin_kind")
	assert.Contains(t, out, "book.name=code_defaults")
}

// TestBootAudit_NilLoggerNoOp pins defensive nil-logger
// handling — production never passes nil, but a misconfigured
// caller shouldn't crash the daemon.
func TestBootAudit_NilLoggerNoOp(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		LogCanonicalRegistryBootAudit(nil, nil, nil)
	})
}

// TestBootAudit_KindAlreadyInRegButNoProvDoesntCrash pins a
// defensive corner: if a kind is in the merged registry but
// missing from the provenance map (shouldn't happen — the merge
// fills both together — but defensive), the audit skips it
// without crashing.
func TestBootAudit_KindAlreadyInRegButNoProvDoesntCrash(t *testing.T) {
	t.Parallel()
	reg := map[string]CanonicalKindConfig{
		"boardgame": {Gaps: map[string]GapSpec{}},
		"ghost":     {Gaps: map[string]GapSpec{}},
	}
	prov := RegistryProvenance{
		"boardgame": {InstructionProvenanceKey: LayerOperatorPerKind, "x": LayerOperatorPerKind},
		// "ghost" omitted intentionally
	}
	logger, buf := newBufferLogger(t, slog.LevelInfo)
	require.NotPanics(t, func() {
		LogCanonicalRegistryBootAudit(logger, reg, prov)
	})
	out := buf.String()
	assert.Contains(t, out, `"kind":"boardgame"`)
	assert.NotContains(t, out, `"kind":"ghost"`)
}
