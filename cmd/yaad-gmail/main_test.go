// CLI surface tests for yaad-gmail. --init emits a populated
// capabilities document (post-— declares source kind, canonical
// kinds, edge types). --version emits the bare version string.
// Default mode without auth env vars emits the config_invalid
// failure envelope.

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/yaad-index/yaad-index/internal/gmail"
)

func TestRun_VersionEmitsBareVersionString(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--version"}, nil, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("--version exit: got %d, want 0; stderr=%q", exit, stderr.String())
	}
	got := strings.TrimRight(stdout.String(), "\n")
	if got == "" {
		t.Fatalf("--version stdout: want non-empty version string")
	}
	if got != gmail.PluginVersion {
		t.Errorf("--version stdout: got %q, want %q", got, gmail.PluginVersion)
	}
}

// TestRun_InitDeclaresKindsAndEdges pins the post- capabilities
// shape: source kind = `gmail`, canonical_kinds_emitted covers
// the email + email-address + label trio, canonical_edge_types_
// emitted covers the seven Gmail edge types, source_namespace =
// `gmail`. URL patterns stay empty (poll-driven plugin).
func TestRun_InitDeclaresKindsAndEdges(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--init"}, nil, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("--init exit: got %d, want 0; stderr=%q", exit, stderr.String())
	}
	var doc capabilitiesDoc
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("--init stdout: not valid JSON: %v; body=%q", err, stdout.String())
	}
	if doc.Name != gmail.PluginName {
		t.Errorf("name: got %q, want %q", doc.Name, gmail.PluginName)
	}
	if doc.Version != gmail.PluginVersion {
		t.Errorf("version: got %q, want %q", doc.Version, gmail.PluginVersion)
	}
	if len(doc.URLPatterns) != 0 {
		t.Errorf("url_patterns: got %v, want [] (poll-driven plugin, no URL form)", doc.URLPatterns)
	}
	if len(doc.EntityKinds) != 1 || doc.EntityKinds[0].Name != gmail.UniversalSourceKind {
		t.Errorf("entity_kinds: got %v, want [{name: %q}]", doc.EntityKinds, gmail.UniversalSourceKind)
	}
	if doc.SourceNamespace != gmail.SourceNamespace {
		t.Errorf("source_namespace: got %q, want %q", doc.SourceNamespace, gmail.SourceNamespace)
	}
	if len(doc.CanonicalKindsEmitted) != 3 {
		t.Errorf("canonical_kinds_emitted: want 3 entries (email + email-address + label), got %v",
			doc.CanonicalKindsEmitted)
	}
	for _, want := range []string{"email", "email-address", "label"} {
		if !contains(doc.CanonicalKindsEmitted, want) {
			t.Errorf("canonical_kinds_emitted missing %q; got %v", want, doc.CanonicalKindsEmitted)
		}
	}
	for _, want := range []string{"is_about", "is_a", "from", "to", "cc", "bcc", "tagged_as"} {
		if !contains(doc.CanonicalEdgeTypesEmitted, want) {
			t.Errorf("canonical_edge_types_emitted missing %q; got %v", want, doc.CanonicalEdgeTypesEmitted)
		}
	}
}

// TestRun_DefaultMode_NoAuthEnv_ReportsConfigInvalid: default mode
// without YAAD_GMAIL_ACCOUNT / YAAD_GMAIL_APP_PASSWORD set in env
// emits the config_invalid failure envelope and exits 1. Pins the
// startup-fatal-error path so a misconfigured operator setup
// surfaces clearly via the daemon's subprocess wrapper rather than
// silently consuming poll cycles.
func TestRun_DefaultMode_NoAuthEnv_ReportsConfigInvalid(t *testing.T) {
	// Not t.Parallel — t.Setenv mutates process env; the Go
	// runtime requires non-parallel scope so the env state is
	// well-defined relative to other tests.
	t.Setenv(EnvAccountEmail, "")
	t.Setenv(EnvAppPassword, "")

	var stdout, stderr bytes.Buffer
	exit := run([]string{}, nil, &stdout, &stderr)
	if exit != 1 {
		t.Fatalf("default mode exit: got %d, want 1; stderr=%q stdout=%q",
			exit, stderr.String(), stdout.String())
	}
	// Per ADR-0023 / yaad-gmail: pre-poll setup failures surface
	// as a single `_error` control packet on stdout, not as the
	// legacy `{ok:false, error, message}` envelope.
	var pkt errorPacket
	if err := json.Unmarshal(stdout.Bytes(), &pkt); err != nil {
		t.Fatalf("default mode stdout: not valid JSON: %v; body=%q", err, stdout.String())
	}
	if pkt.Error.Kind != "config_invalid" {
		t.Errorf("_error.kind: got %q, want %q", pkt.Error.Kind, "config_invalid")
	}
	if pkt.Error.Message == "" {
		t.Errorf("_error.message: want non-empty; got %q", pkt.Error.Message)
	}
}

func TestRun_BadFlagsExitTwo(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--unknown-flag"}, nil, &stdout, &stderr)
	if exit != 2 {
		t.Errorf("bad flags exit: got %d, want 2", exit)
	}
}

// TestRun_InitDeclaresFetchCommand pins the post- capabilities
// surface: --init's `commands` field includes "fetch" so
// yaad-index's routing-time validation accepts
// `gmail: !fetch` as a legal command-shape input.
func TestRun_InitDeclaresFetchCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--init"}, nil, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("--init exit: got %d, want 0; stderr=%q", exit, stderr.String())
	}
	var doc capabilitiesDoc
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("--init stdout: not valid JSON: %v; body=%q", err, stdout.String())
	}
	if !contains(doc.Commands, "fetch") {
		t.Errorf("commands: want [\"fetch\"], got %v", doc.Commands)
	}
}

// TestRun_UnknownCommandExitsTwo pins that an unknown --command
// value rejects with exit code 2 (bad-flag class). yaad-index
// routing-time validation should catch most of these before the
// daemon spawns the subprocess; this test pins the binary-side
// defense.
func TestRun_UnknownCommandExitsTwo(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--command", "no-such-command"}, nil, &stdout, &stderr)
	if exit != 2 {
		t.Errorf("unknown --command exit: got %d, want 2; stderr=%q", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no-such-command") {
		t.Errorf("stderr should name the rejected command; got %q", stderr.String())
	}
}

// TestRun_CommandFetchNoAuthSurfaceErrorPacket pins that
// `--command fetch` without auth env vars emits a `_error` line
// (matches the no-args-default behavior — both routes call
// runCommandFetch and surface the same shape).
func TestRun_CommandFetchNoAuthSurfaceErrorPacket(t *testing.T) {
	// Not t.Parallel — t.Setenv mutates process env.
	t.Setenv(EnvAccountEmail, "")
	t.Setenv(EnvAppPassword, "")

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--command", "fetch"}, nil, &stdout, &stderr)
	if exit != 1 {
		t.Fatalf("--command fetch (no auth) exit: got %d, want 1; stderr=%q stdout=%q",
			exit, stderr.String(), stdout.String())
	}
	var pkt errorPacket
	if err := json.Unmarshal(stdout.Bytes(), &pkt); err != nil {
		t.Fatalf("stdout: not valid JSON: %v; body=%q", err, stdout.String())
	}
	if pkt.Error.Kind != "config_invalid" {
		t.Errorf("_error.kind: got %q, want %q", pkt.Error.Kind, "config_invalid")
	}
}

// TestBuildSourceLine_ShapeMatchesADR0021 pins the per-message
// NDJSON wire shape `runCommandFetch` emits per ADR-0021. The
// source emission carries top-level `ok=true`, structured.kind =
// "source", structured.name = the slug-only form (no namespace
// prefix), structured.data with subject + date (metadata only),
// the edges block keyed by edge type, and the email body on
// top-level `raw_content` for the daemon to wrap in yaad:plugin
// markers in the entity's .md body per ADR-0015.
func TestBuildSourceLine_ShapeMatchesADR0021(t *testing.T) {
	t.Parallel()

	env := gmail.IngestEnvelope{
		SourceID: "gmail:msg-foo-dot-com",
		Subject: "Test subject",
		Body: []byte("body content"),
		Edges: []gmail.Edge{
			{Type: "from", Name: "alice@example.com", Kind: "email-address"},
			{Type: "to", Name: "bob@example.com", Kind: "email-address"},
		},
	}
	got := buildSourceLine(env, "2026-05-10T17:00:00Z")

	if !got.OK {
		t.Errorf("ok: want true")
	}
	if got.Structured == nil {
		t.Fatal("structured: want non-nil")
	}
	if got.Structured.Kind != "source" {
		t.Errorf("structured.kind: got %q, want %q", got.Structured.Kind, "source")
	}
	if got.Structured.Name != "msg-foo-dot-com" {
		t.Errorf("structured.name: got %q, want %q (slug-only, no `gmail:` prefix)",
			got.Structured.Name, "msg-foo-dot-com")
	}
	if got.Structured.Data["subject"] != "Test subject" {
		t.Errorf("structured.data.subject: got %v", got.Structured.Data["subject"])
	}
	if got.RawContent != "body content" {
		t.Errorf("raw_content: got %q, want %q (body lives on top-level raw_content, not data.body)",
			got.RawContent, "body content")
	}
	if _, hasBody := got.Structured.Data["body"]; hasBody {
		t.Errorf("structured.data.body: must not be set; body belongs on top-level raw_content per ADR-0008/ADR-0015")
	}
	if len(got.Structured.Edges["from"]) != 1 || got.Structured.Edges["from"][0].Name != "alice@example.com" {
		t.Errorf("structured.edges[from]: got %v", got.Structured.Edges["from"])
	}
	if len(got.Structured.Provenance) != 1 || got.Structured.Provenance[0].Source != "gmail:fetch" {
		t.Errorf("structured.provenance: got %v", got.Structured.Provenance)
	}
}

// TestBuildSourceLine_EmptyBodyOmitsRawContent confirms an
// envelope with no body produces an empty top-level raw_content
// (which json's omitempty drops on the wire) and leaves data
// without a body key.
func TestBuildSourceLine_EmptyBodyOmitsRawContent(t *testing.T) {
	t.Parallel()

	env := gmail.IngestEnvelope{
		SourceID: "gmail:msg-empty",
		Subject: "subject only",
	}
	got := buildSourceLine(env, "2026-05-10T17:00:00Z")

	if got.RawContent != "" {
		t.Errorf("raw_content: got %q, want empty", got.RawContent)
	}
	if _, hasBody := got.Structured.Data["body"]; hasBody {
		t.Errorf("structured.data.body: must not be set")
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(got); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"raw_content"`)) {
		t.Errorf("omitempty must drop empty raw_content from wire: %s", buf.String())
	}
}

// TestBuildSourceLine_NDJSONEncodingShape pins the wire-format
// contract: json.NewEncoder.Encode produces a single line + trailing
// `\n`, which is what ADR-0023's daemon-side json.Decoder consumes
// per envelope. yaad-wikipedia / yaad-bgg's NDJSON migrations pinned
// this for their wire shapes; yaad-gmail follows suit.
func TestBuildSourceLine_NDJSONEncodingShape(t *testing.T) {
	t.Parallel()

	env := gmail.IngestEnvelope{
		SourceID: "gmail:msg-x",
		Subject: "subject",
		Body: []byte("body with\nembedded newline"),
	}
	line := buildSourceLine(env, "2026-05-10T17:00:00Z")

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(line); err != nil {
		t.Fatalf("encode: %v", err)
	}

	out := buf.Bytes()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Errorf("must end with `\\n`; got %q", out)
	}
	// Exactly one newline total — the embedded newline in `body`
	// gets JSON-escaped as `\n` literal inside the string value,
	// not a raw newline on the wire.
	if got := bytes.Count(out, []byte("\n")); got != 1 {
		t.Errorf("must be single-line NDJSON (one newline at end); got %d newlines, raw=%q",
			got, out)
	}
}

// TestErrorPacket_NDJSONEncodingShape pins the same NDJSON-shape
// contract for `_error` control packets — single-line + trailing
// `\n` so the daemon's json.Decoder consumes one packet per call
// to Decode.
func TestErrorPacket_NDJSONEncodingShape(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeErrorPacket(&buf, "test_kind", "test message")

	out := buf.Bytes()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Errorf("must end with `\\n`; got %q", out)
	}
	if got := bytes.Count(out, []byte("\n")); got != 1 {
		t.Errorf("must be single-line NDJSON; got %d newlines, raw=%q", got, out)
	}

	var pkt errorPacket
	if err := json.Unmarshal(out, &pkt); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pkt.Error.Kind != "test_kind" || pkt.Error.Message != "test message" {
		t.Errorf("decode round-trip failed: got %+v", pkt)
	}
}

// contains is a tiny slice-membership helper for the table-style
// assertions above.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestParseLogLevel pins the env-driven log-level mapping per #54.
// debug / info / warn / warning / error map to the matching
// slog.Level; unknown / empty / whitespace values fall back to
// info (lenient: a typo in the env var must NOT block a fetch
// cycle, the worst case is "operator gets info-level instead of
// the level they wanted").
func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"  Debug  ", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"trace", slog.LevelInfo}, // unknown → fallback
		{"DEBUG ME", slog.LevelInfo}, // unknown (whitespace inside) → fallback
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseLogLevel(tc.in)
			if got != tc.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildSourceLine_AddressFields_PopulatedFromEnvelope pins
// #272: buildSourceLine surfaces from/to/cc/bcc into the
// structured.data map so workflow CEL predicates can read
// `entity.data.from == "noreply@example.com"` etc. directly.
// to / cc render as `[]` (not null) for stable shape; bcc is
// omitted when empty (sent-folder-only field).
func TestBuildSourceLine_AddressFields_PopulatedFromEnvelope(t *testing.T) {
	t.Parallel()

	env := gmail.IngestEnvelope{
		SourceID: "gmail:msg-x",
		Subject:  "subject",
		From:     "alice@example.com",
		To:       []string{"bob@example.com", "carol@example.com"},
		Cc:       []string{"dave@example.com"},
		Bcc:      nil,
	}
	got := buildSourceLine(env, "2099-05-25T12:00:00Z")
	d := got.Structured.Data

	if d["from"] != "alice@example.com" {
		t.Errorf("data.from: got %v, want alice@example.com", d["from"])
	}
	toSlice, ok := d["to"].([]string)
	if !ok || len(toSlice) != 2 {
		t.Errorf("data.to: got %v (%T), want []string of len 2", d["to"], d["to"])
	}
	ccSlice, ok := d["cc"].([]string)
	if !ok || len(ccSlice) != 1 {
		t.Errorf("data.cc: got %v (%T), want []string of len 1", d["cc"], d["cc"])
	}
	if _, hasBcc := d["bcc"]; hasBcc {
		t.Errorf("data.bcc: must be omitted when envelope Bcc is empty")
	}
}

// TestBuildSourceLine_AddressFields_ShapeStableForAbsentHeaders
// pins the absent-header degradation: missing From omits the
// field; missing To / Cc still emit `[]` for shape stability
// (CEL `entity.data.to` always traverses cleanly).
func TestBuildSourceLine_AddressFields_ShapeStableForAbsentHeaders(t *testing.T) {
	t.Parallel()

	env := gmail.IngestEnvelope{
		SourceID: "gmail:msg-y",
		Subject:  "subject",
	}
	got := buildSourceLine(env, "2099-05-25T12:00:00Z")
	d := got.Structured.Data

	if _, hasFrom := d["from"]; hasFrom {
		t.Errorf("data.from: must be omitted when envelope From is empty")
	}
	toSlice, ok := d["to"].([]string)
	if !ok || len(toSlice) != 0 {
		t.Errorf("data.to: got %v (%T), want empty []string", d["to"], d["to"])
	}
	ccSlice, ok := d["cc"].([]string)
	if !ok || len(ccSlice) != 0 {
		t.Errorf("data.cc: got %v (%T), want empty []string", d["cc"], d["cc"])
	}
}

// TestBuildSourceLine_AddressFields_SentFolderBcc pins that a
// sent-folder message's Bcc list surfaces on data.bcc.
func TestBuildSourceLine_AddressFields_SentFolderBcc(t *testing.T) {
	t.Parallel()

	env := gmail.IngestEnvelope{
		SourceID: "gmail:msg-sent",
		Subject:  "sent",
		From:     "operator@example.com",
		Bcc:      []string{"hidden@example.com"},
	}
	got := buildSourceLine(env, "2099-05-25T12:00:00Z")
	bccSlice, ok := got.Structured.Data["bcc"].([]string)
	if !ok || len(bccSlice) != 1 || bccSlice[0] != "hidden@example.com" {
		t.Errorf("data.bcc: got %v (%T), want [hidden@example.com]", got.Structured.Data["bcc"], got.Structured.Data["bcc"])
	}
}
