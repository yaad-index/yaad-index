package plugins

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseInvocation_CommandShape pins the command-shape recognition
// per ADR-0022 §2: `<plugin>: !<command>` parses as InvocationCommand
// with Plugin + Command populated, sigil stripped.
func TestParseInvocation_CommandShape(t *testing.T) {
	cases := []struct {
		name string
		input string
		wantPlugin string
		wantCommand string
	}{
		{"canonical form", "gmail: !fetch", "gmail", "fetch"},
		{"no space after colon", "gmail:!fetch", "gmail", "fetch"},
		{"extra whitespace after bang", "gmail: ! fetch ", "gmail", "fetch"},
		{"tab between colon and bang", "gmail:\t!fetch", "gmail", "fetch"},
		{"hyphenated plugin name", "yaad-gmail: !fetch", "yaad-gmail", "fetch"},
		{"future bgg sync example", "bgg: !sync", "bgg", "sync"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseInvocation(tc.input)
			require.Equal(t, InvocationCommand, got.Shape, "expected command-shape for %q", tc.input)
			require.Equal(t, tc.wantPlugin, got.Plugin)
			require.Equal(t, tc.wantCommand, got.Command)
			require.Empty(t, got.URL, "URL must not be populated on command-shape")
		})
	}
}

// TestParseInvocation_URLShape pins that all non-bang inputs fall
// through to URL-shape with URL = input verbatim. ADR-0022 §2 makes
// the `!` the sole discriminator; everything else routes through the
// existing url_patterns dispatch path.
func TestParseInvocation_URLShape(t *testing.T) {
	cases := []struct {
		name string
		input string
	}{
		{"wikipedia shorthand", "wikipedia: Tehran"},
		{"bgg shorthand", "bgg: ticket to ride"},
		{"full https URL", "https://en.wikipedia.org/wiki/Tehran"},
		{"full http URL", "http://example.org/path"},
		{"plain text without colon", "no colon here"},
		{"empty string", ""},
		{"whitespace only", " "},
		{"colon with nothing after bang", "gmail: !"},
		{"colon with whitespace-only after bang", "gmail: ! "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseInvocation(tc.input)
			require.Equal(t, InvocationURL, got.Shape, "expected URL-shape for %q", tc.input)
			require.Equal(t, tc.input, got.URL, "URL must preserve input verbatim")
			require.Empty(t, got.Command, "Command must not be populated on URL-shape")
		})
	}
}

// TestParseInvocation_BangAfterColonAlwaysCommand pins the parser's
// hard rule: the `!` sigil after `<plugin>:` is the discriminator,
// regardless of the command name's contents. Routing-time validation
// is what rejects unknown commands; the parser doesn't know
// the plugin's `commands` list and doesn't try to.
func TestParseInvocation_BangAfterColonAlwaysCommand(t *testing.T) {
	got := ParseInvocation("wikipedia: !made-up-command")
	require.Equal(t, InvocationCommand, got.Shape)
	require.Equal(t, "wikipedia", got.Plugin)
	require.Equal(t, "made-up-command", got.Command)
}

// TestParseInvocation_FullURLFallsThrough pins the URL-shape contract
// for a full HTTPS URL. The colon-prefix split DOES populate
// Invocation.Plugin with the URL scheme (`https`) — that's the best-
// effort metadata documented in splitPluginPrefix's godoc. The load-
// bearing contract is that URL is preserved verbatim so the existing
// url_patterns regex matchers see the unmodified input.
func TestParseInvocation_FullURLFallsThrough(t *testing.T) {
	got := ParseInvocation("https://example.org/path")
	require.Equal(t, InvocationURL, got.Shape)
	require.Equal(t, "https", got.Plugin,
		"URL-shape Plugin field is best-effort metadata = the colon-prefix token (here, the URL scheme)")
	require.Equal(t, "https://example.org/path", got.URL,
		"URL must be preserved verbatim — load-bearing for the url_patterns regex matchers")
}

// TestCapabilities_CommandsRoundTrip pins the JSON round-trip of the
// new `commands` field added by ADR-0022 / yaad-index. This is
// the same path the capability cache walks: subprocess --init JSON →
// plugins.Capabilities → store-roundtrip JSON → plugins.Capabilities.
// The opaque-JSON store contract means a new field plumbs through
// automatically as long as the struct decodes + encodes it; this test
// pins that contract.
func TestCapabilities_CommandsRoundTrip(t *testing.T) {
	in := Capabilities{
		Name: "gmail",
		Version: "0.3.0",
		URLPatterns: []string{},
		EntityKinds: []KindSpec{{Name: "source"}},
		EdgeKinds: []KindSpec{},
		SourceNamespace: "gmail",
		Commands: []CommandSpec{{Name: "fetch"}},
	}
	body, err := json.Marshal(in)
	require.NoError(t, err)
	require.Contains(t, string(body), `"commands":["fetch"]`,
		"commands field must serialize on the wire when populated; default-shape "+
			"OperatorOnly=false emits the bare-string form for back-compat")

	var out Capabilities
	require.NoError(t, json.Unmarshal(body, &out))
	require.Equal(t, []CommandSpec{{Name: "fetch"}}, out.Commands,
		"commands must round-trip through JSON unchanged")
}

// TestCapabilities_CommandsOperatorOnly_RoundTrip pins the long-form
// wire shape for #107: a command with operator_only=true serializes
// as the object form and decodes back with the flag preserved.
func TestCapabilities_CommandsOperatorOnly_RoundTrip(t *testing.T) {
	in := Capabilities{
		Name: "gmail",
		Commands: []CommandSpec{
			{Name: "fetch"},
			{Name: "delete-all", OperatorOnly: true},
		},
	}
	body, err := json.Marshal(in)
	require.NoError(t, err)
	require.Contains(t, string(body), `"fetch"`,
		"default-shape command stays as bare string")
	require.Contains(t, string(body), `"name":"delete-all"`,
		"operator-only command serializes as the object form")
	require.Contains(t, string(body), `"operator_only":true`,
		"operator_only flag must serialize on the wire when true")

	var out Capabilities
	require.NoError(t, json.Unmarshal(body, &out))
	require.Equal(t, []CommandSpec{
		{Name: "fetch"},
		{Name: "delete-all", OperatorOnly: true},
	}, out.Commands)
}

// TestCapabilities_CommandsStringShorthand pins decoding of a pre-#107
// plugin emitting bare strings: the daemon must accept the legacy
// shape unchanged + default OperatorOnly to false.
func TestCapabilities_CommandsStringShorthand(t *testing.T) {
	const wire = `{"name":"gmail","commands":["fetch","sync"]}`
	var caps Capabilities
	require.NoError(t, json.Unmarshal([]byte(wire), &caps))
	require.Equal(t, []CommandSpec{
		{Name: "fetch"},
		{Name: "sync"},
	}, caps.Commands,
		"bare-string commands must decode with OperatorOnly=false (agent-callable)")
}

// TestCapabilities_CommandsBackCompat pins back-compat: a plugin
// predating ADR-0022 emits no `commands` field and the daemon must
// decode it as nil (Go zero value). Per ADR-0022's plugin-side
// migration section, no-commands plugins (yaad-wikipedia, yaad-bgg)
// need zero changes.
func TestCapabilities_CommandsBackCompat(t *testing.T) {
	const preADR = `{
		"name": "wikipedia",
		"version": "0.4.0",
		"url_patterns": ["^https?://[a-z]{2,3}\\.wikipedia\\.org/wiki/.+"],
		"entity_kinds": [{"name": "wikipedia"}],
		"edge_kinds": []
	}`
	var caps Capabilities
	require.NoError(t, json.Unmarshal([]byte(preADR), &caps))
	require.Nil(t, caps.Commands,
		"absent commands field must decode as nil — back-compat with pre-ADR-0022 plugins")
}

// TestCapabilities_CommandsOmitemptyWhenAbsent pins the omitempty tag:
// a plugin that doesn't set Commands shouldn't ship an empty
// `"commands": null` (or `[]`) on the wire. Keeps the no-commands
// case byte-identical to the pre-ADR-0022 wire shape so the
// reprobe-shape diff doesn't false-positive on every cache row.
func TestCapabilities_CommandsOmitemptyWhenAbsent(t *testing.T) {
	caps := Capabilities{
		Name: "wikipedia",
		Version: "0.4.0",
	}
	body, err := json.Marshal(caps)
	require.NoError(t, err)
	require.NotContains(t, string(body), "commands",
		"omitempty must drop the field when Commands is nil — preserves back-compat shape")
}

// TestCapabilities_DateFieldsRoundTrip pins the ADR-0025 cut 2
// (#221) wire shape: the `date_fields` block round-trips through
// JSON unchanged.
func TestCapabilities_DateFieldsRoundTrip(t *testing.T) {
	in := Capabilities{
		Name: "calendar",
		DateFields: map[string]string{
			"start": "occurred_on",
			"due":   "due_on",
		},
	}
	body, err := json.Marshal(in)
	require.NoError(t, err)
	require.Contains(t, string(body), `"date_fields"`,
		"date_fields field must serialize when populated")

	var out Capabilities
	require.NoError(t, json.Unmarshal(body, &out))
	require.Equal(t, in.DateFields, out.DateFields)
}

// TestCapabilities_DateFieldsOmitemptyWhenAbsent pins that plugins
// predating #221 (no date_fields declaration) emit no field on
// the wire — preserves back-compat for every existing plugin.
func TestCapabilities_DateFieldsOmitemptyWhenAbsent(t *testing.T) {
	caps := Capabilities{Name: "wikipedia"}
	body, err := json.Marshal(caps)
	require.NoError(t, err)
	require.NotContains(t, string(body), "date_fields",
		"omitempty must drop the field when DateFields is nil")
}

// --- ADR-0028 Cut 4: `<plugin>/<instance>` grammar ---

func TestParseInvocation_InstanceScopedCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input            string
		wantPlugin       string
		wantInstance     string
		wantCommand      string
	}{
		{"gmail/personal: !fetch", "gmail", "personal", "fetch"},
		{"github/acme-org: !fetch", "github", "acme-org", "fetch"},
		{"github/acme-org:!fetch", "github", "acme-org", "fetch"}, // no space
		{"plugin/inst: !cmd arg-ignored", "plugin", "inst", "cmd arg-ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := ParseInvocation(tc.input)
			require.Equal(t, InvocationCommand, got.Shape, "expected command-shape for %q", tc.input)
			assert.Equal(t, tc.wantPlugin, got.Plugin)
			assert.Equal(t, tc.wantInstance, got.Instance)
			assert.Equal(t, tc.wantCommand, got.Command)
		})
	}
}

// TestParseInvocation_BarePluginNoInstance pins the back-compat
// path: bare `<plugin>: !<cmd>` still parses with empty Instance
// (the dispatch layer fans out across enabled instances in this case).
func TestParseInvocation_BarePluginNoInstance(t *testing.T) {
	t.Parallel()
	got := ParseInvocation("gmail: !fetch")
	require.Equal(t, InvocationCommand, got.Shape)
	assert.Equal(t, "gmail", got.Plugin)
	assert.Equal(t, "", got.Instance)
	assert.Equal(t, "fetch", got.Command)
}

// TestParseInvocation_EmptyInstanceFallsToURL pins the
// shape-rejection: a malformed `/foo:` or `foo/:` prefix doesn't
// silently mis-parse as command-shape. URL-shape path takes over
// so the existing "no plugin handles URL" guard rejects cleanly.
func TestParseInvocation_EmptyInstanceFallsToURL(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"/personal: !fetch",  // empty plugin half
		"gmail/: !fetch",     // empty instance half
		"/: !fetch",          // both empty
	} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			got := ParseInvocation(bad)
			assert.Equal(t, InvocationURL, got.Shape,
				"malformed plugin/instance prefix must NOT parse as command-shape")
		})
	}
}
