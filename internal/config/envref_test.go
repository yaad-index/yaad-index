package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpandEnvReferences_LiteralPassThrough pins the no-op
// path: a value with no `${...}` reference returns verbatim.
func TestExpandEnvReferences_LiteralPassThrough(t *testing.T) {
	t.Parallel()
	got, emptyRefs, err := ExpandEnvReferences("literal-value-no-refs")
	require.NoError(t, err)
	assert.Equal(t, "literal-value-no-refs", got)
	assert.Empty(t, emptyRefs)
}

// TestExpandEnvReferences_BareDollarPassThrough pins that bare
// `$NAME` (shell shorthand) is NOT expanded — only `${NAME}`
// per the strict syntax. PATs / API keys that legitimately
// contain `$` characters pass through unchanged.
func TestExpandEnvReferences_BareDollarPassThrough(t *testing.T) {
	t.Parallel()
	got, _, err := ExpandEnvReferences("token$with$dollar$chars")
	require.NoError(t, err)
	assert.Equal(t, "token$with$dollar$chars", got)
}

// TestExpandEnvReferences_HappyPath pins the basic substitution
// shape: a single `${NAME}` reference resolves to the env
// var's value.
//
// t.Setenv-using tests intentionally don't call t.Parallel —
// Setenv panics under parallel because process env is shared
// global mutable state.
func TestExpandEnvReferences_HappyPath(t *testing.T) {
	t.Setenv("YAAD_ENVREF_HAPPY", "resolved-value")
	got, emptyRefs, err := ExpandEnvReferences("${YAAD_ENVREF_HAPPY}")
	require.NoError(t, err)
	assert.Equal(t, "resolved-value", got)
	assert.Empty(t, emptyRefs)
}

// TestExpandEnvReferences_MixedLiteralAndReference pins that
// literal text + references in the same value compose: a token
// like `prefix-${VAR}-suffix` expands the middle and leaves
// the surrounding literal intact.
func TestExpandEnvReferences_MixedLiteralAndReference(t *testing.T) {
	t.Setenv("YAAD_ENVREF_MIXED", "MIDDLE")
	got, _, err := ExpandEnvReferences("prefix-${YAAD_ENVREF_MIXED}-suffix")
	require.NoError(t, err)
	assert.Equal(t, "prefix-MIDDLE-suffix", got)
}

// TestExpandEnvReferences_MultipleReferencesInValue pins that
// multiple references in a single value all expand — useful
// for composite tokens (rare but legal).
func TestExpandEnvReferences_MultipleReferencesInValue(t *testing.T) {
	t.Setenv("YAAD_ENVREF_A", "alpha")
	t.Setenv("YAAD_ENVREF_B", "beta")
	got, _, err := ExpandEnvReferences("${YAAD_ENVREF_A}::${YAAD_ENVREF_B}")
	require.NoError(t, err)
	assert.Equal(t, "alpha::beta", got)
}

// TestExpandEnvReferences_MissingReferenceFailsFast pins the
// fail-fast contract: an unresolved `${NAME}` returns a wrapped
// ErrUnresolvedEnvReference + names the missing variable in the
// error message.
func TestExpandEnvReferences_MissingReferenceFailsFast(t *testing.T) {
	t.Parallel()
	// No t.Setenv — the var is genuinely absent.
	_, _, err := ExpandEnvReferences("${YAAD_ENVREF_NONEXISTENT}")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnresolvedEnvReference))
	assert.Contains(t, err.Error(), "YAAD_ENVREF_NONEXISTENT")
}

// TestExpandEnvReferences_EmptyValueReturnsEmptyRef pins the
// "env var present but value is empty" path: expansion
// succeeds + the empty-refs slice names the var so the caller
// can warn.
func TestExpandEnvReferences_EmptyValueReturnsEmptyRef(t *testing.T) {
	t.Setenv("YAAD_ENVREF_EMPTY", "")
	got, emptyRefs, err := ExpandEnvReferences("prefix-${YAAD_ENVREF_EMPTY}-suffix")
	require.NoError(t, err)
	assert.Equal(t, "prefix--suffix", got, "empty expansion leaves the surrounding literal")
	assert.Equal(t, []string{"YAAD_ENVREF_EMPTY"}, emptyRefs)
}

// TestExpandEnvReferences_NoFallbackSyntax pins the
// out-of-scope shell-style `${VAR:-default}` shape — the parser
// doesn't recognize the `:-` operator, so the whole expression
// is treated as a non-match (literal pass-through).
func TestExpandEnvReferences_NoFallbackSyntax(t *testing.T) {
	t.Parallel()
	// Shell syntax with a colon-dash would resolve to "default"
	// in bash; here it's a literal that doesn't match the
	// envRefPattern (the `:` breaks the identifier match).
	// The whole literal passes through unchanged.
	got, _, err := ExpandEnvReferences("${VAR:-default}")
	require.NoError(t, err)
	assert.Equal(t, "${VAR:-default}", got,
		"v1 shape is strict ${NAME} only; ${VAR:-default} is out of scope")
}

// TestExpandEnvReferences_NoNestedExpansion pins the
// one-level-only rule: if an env var's value itself contains
// `${...}`, expansion stops after one level (the value passes
// through verbatim, including the inner `${...}` literal).
func TestExpandEnvReferences_NoNestedExpansion(t *testing.T) {
	t.Setenv("YAAD_ENVREF_OUTER", "literal-with-${YAAD_ENVREF_INNER}-inside")
	t.Setenv("YAAD_ENVREF_INNER", "should-not-resolve")
	got, _, err := ExpandEnvReferences("${YAAD_ENVREF_OUTER}")
	require.NoError(t, err)
	assert.True(t, strings.Contains(got, "${YAAD_ENVREF_INNER}"),
		"nested ${...} in the resolved value must NOT trigger a second expansion pass")
}
