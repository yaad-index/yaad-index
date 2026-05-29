package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/yaad-index/yaad-index/internal/bgg"
)

// TestRenderBody_HappyPath pins's expected body shape:
// title H1 + blank line + image embed (matching the daemon's
// ADR-0018 §Attachments nested vault layout) + blank line +
// description prose.
func TestRenderBody_HappyPath(t *testing.T) {
	t.Parallel()

	bg := &bgg.Boardgame{
		Name: "Brass: Birmingham (2018)",
		Data: map[string]any{
			"title": "Brass: Birmingham",
			"description": "Brass: Birmingham is an economic strategy game.",
		},
	}
	got := renderBody(bg, "brass-birmingham-2018", "jpg")
	want := "# Brass: Birmingham\n" +
		"\n![thumbnail](brass-birmingham-2018/attachments/thumb.jpg)\n" +
		"\nBrass: Birmingham is an economic strategy game.\n"
	if got != want {
		t.Errorf("renderBody happy path:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

// TestRenderBody_NoThumbnail covers the BGG-no-thumbnail path: the
// staging step skipped (thumbExt=""), so the body has H1 +
// description but NO image embed line.
func TestRenderBody_NoThumbnail(t *testing.T) {
	t.Parallel()

	bg := &bgg.Boardgame{
		Name: "Obscure Game (2024)",
		Data: map[string]any{"title": "Obscure Game", "description": "tiny indie game."},
	}
	got := renderBody(bg, "obscure-game-2024", "")
	want := "# Obscure Game\n\ntiny indie game.\n"
	if got != want {
		t.Errorf("renderBody no-thumb:\nwant %q\ngot %q", want, got)
	}
	if strings.Contains(got, "![") {
		t.Errorf("no-thumb body must NOT contain image embed; got %q", got)
	}
}

// TestRenderBody_NoDescription covers BGG-no-description: H1 +
// image embed with no description prose. Body still renders cleanly.
func TestRenderBody_NoDescription(t *testing.T) {
	t.Parallel()

	bg := &bgg.Boardgame{
		Name: "No-Desc Game (2024)",
		Data: map[string]any{"title": "No-Desc Game"},
	}
	got := renderBody(bg, "no-desc-2024", "png")
	want := "# No-Desc Game\n\n![thumbnail](no-desc-2024/attachments/thumb.png)\n"
	if got != want {
		t.Errorf("renderBody no-desc:\nwant %q\ngot %q", want, got)
	}
}

// TestRenderBody_DescriptionAlreadyTrailingNewline pins the no-
// double-newline guarantee: BGG descriptions sometimes end with a
// newline already; renderBody MUST NOT add a second one.
func TestRenderBody_DescriptionAlreadyTrailingNewline(t *testing.T) {
	t.Parallel()

	bg := &bgg.Boardgame{
		Name: "Any Game (2024)",
		Data: map[string]any{
			"title": "Any Game",
			"description": "Pre-newlined description.\n",
		},
	}
	got := renderBody(bg, "any-2024", "")
	if strings.HasSuffix(got, "\n\n") {
		t.Errorf("renderBody must not double-trailing-newline; got %q", got)
	}
	if !strings.HasSuffix(got, "Pre-newlined description.\n") {
		t.Errorf("renderBody must end with the description's existing newline; got %q", got)
	}
}

// TestRenderBody_ImageEmbedFilenameMatchesADR0018 pins the image-
// embed path against the daemon's ADR-0018 §Attachments nested
// layout (`<local-id>/attachments/thumb.<ext>`). Mismatch here means
// Obsidian / markdown renderers fail to resolve the image even
// though the daemon wrote the file correctly to its new home.
func TestRenderBody_ImageEmbedFilenameMatchesADR0018(t *testing.T) {
	t.Parallel()

	bg := &bgg.Boardgame{
		Name: "Risk (1959)",
		Data: map[string]any{"title": "Risk", "description": "x"},
	}
	got := renderBody(bg, "risk-1959", "jpg")
	if !strings.Contains(got, "![thumbnail](risk-1959/attachments/thumb.jpg)") {
		t.Errorf("image embed must match ADR-0018 <local-id>/attachments/<role>.<ext> path; got %q", got)
	}
}

// TestRunInit_EmitsCapabilities exercises the `--init` mode end-to-end:
// the JSON written to stdout must conform to the capabilities document
// shape yaad-index's subprocess.New consumes (per
// internal/plugins/subprocess/subprocess.go in yaad-index). If this
// test drifts from the index's expectations, the loop breaks at
// startup — not at first /v1/ingest — so it stays a sharp regression
// signal.
func TestRunInit_EmitsCapabilities(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	if err := runInit(&stdout); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	var got capabilitiesDoc
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode capabilities JSON: %v\nraw: %s", err, stdout.String())
	}

	if got.Name != bgg.PluginName {
		t.Errorf("name: want %q, got %q", bgg.PluginName, got.Name)
	}
	if got.Version == "" {
		t.Errorf("version: want non-empty, got %q", got.Version)
	}
	if len(got.URLPatterns) < 2 {
		t.Fatalf("url_patterns: want at least two (canonical URL + shorthand), got %d", len(got.URLPatterns))
	}
	// Compile each pattern — yaad-index will compile these at startup
	// and a malformed regex would fail-fast there. Catching it here
	// keeps the surprise local.
	for _, pat := range got.URLPatterns {
		if _, err := regexp.Compile(pat); err != nil {
			t.Errorf("url_pattern %q: did not compile: %v", pat, err)
		}
	}
	// At least one pattern must claim canonical BGG URLs and at least
	// one must claim the shorthand. We don't check which pattern does
	// which (that's an implementation detail) — only that the union
	// covers both shapes and rejects obvious non-matches.
	matchAny := func(s string) bool {
		for _, pat := range got.URLPatterns {
			re, err := regexp.Compile(pat)
			if err != nil {
				continue
			}
			if re.MatchString(s) {
				return true
			}
		}
		return false
	}
	if !matchAny("https://boardgamegeek.com/boardgame/224517/brass-birmingham") {
		t.Errorf("url_patterns: should match canonical boardgamegeek.com URL across the union")
	}
	if !matchAny("bgg: 224517") {
		t.Errorf("url_patterns: should match shorthand `bgg: <id>` across the union")
	}
	if matchAny("https://example.com/boardgame/224517") {
		t.Errorf("url_patterns: should NOT match example.com")
	}
	if matchAny("https://boardgamegeek.com/forum/123") {
		t.Errorf("url_patterns: should NOT match BGG forum URLs (out of /boardgame/ scope)")
	}

	// Per ADR-0021: yaad-bgg emits the universal `kind: source`
	// shape; entity_kinds collapses to a single source kind.
	// Daemon's vault-path routing uses source_namespace.
	if len(got.EntityKinds) != 1 || got.EntityKinds[0].Name != bgg.UniversalSourceKind {
		t.Errorf("entity_kinds: want exactly one %q, got %+v",
			bgg.UniversalSourceKind, got.EntityKinds)
	}
	if got.EntityKinds[0].DefaultTTLDays <= 0 {
		t.Errorf("entity_kinds[0].default_ttl_days: want > 0, got %d", got.EntityKinds[0].DefaultTTLDays)
	}
	if len(got.EdgeKinds) != 0 {
		t.Errorf("edge_kinds: want empty for v1, got %+v", got.EdgeKinds)
	}

	// Per yaad-index/yaad-bgg: top-level cache_ttl_seconds = 365 days
	// participates in yaad-index's three-level cache resolution
	// at the plugin level. Same contract as.
	if got.CacheTTLSeconds != bgg.DefaultCacheTTLSeconds {
		t.Errorf("cache_ttl_seconds: want %d (DefaultCacheTTLSeconds = 365d), got %d",
			bgg.DefaultCacheTTLSeconds, got.CacheTTLSeconds)
	}

	// Canonical-kinds declaration: BGG's primary surface is the
	// boardgame itself, so the canonical_kinds_emitted set contains
	// Per ADR-0021: yaad-bgg declares canonical kinds the source
	// node's edges point at — boardgame (canonical-kind-edge
	// `is_about` target), person (designed_by + artist_by
	// targets), company (published_by target).
	wantKinds := map[string]bool{
		bgg.CanonicalKind: false,
		"person": false,
		"company": false,
	}
	for _, k := range got.CanonicalKindsEmitted {
		if _, ok := wantKinds[k]; ok {
			wantKinds[k] = true
		}
	}
	for k, present := range wantKinds {
		if !present {
			t.Errorf("canonical_kinds_emitted: missing %q (got %+v)",
				k, got.CanonicalKindsEmitted)
		}
	}

	// #332: resolver capability for boardgame only — BGG's
	// search-by-name surface is reliable for boardgame ids;
	// person / company stay off the resolver claim until the
	// plugin's search proves out for those kinds. The subset-of-
	// emitted constraint (daemon-side ERROR if a resolves entry
	// isn't also emitted) is verified by daemon startup; this
	// test pins the wire-shape minimum the plugin must declare.
	wantResolves := map[string]bool{
		bgg.CanonicalKind: false,
	}
	for _, k := range got.ResolvesCanonicalKinds {
		if _, ok := wantResolves[k]; ok {
			wantResolves[k] = true
		}
	}
	for k, present := range wantResolves {
		if !present {
			t.Errorf("resolves_canonical_kinds: missing %q (got %+v)",
				k, got.ResolvesCanonicalKinds)
		}
	}
	// Conservative scope: must NOT claim person/company until
	// BGG's search proves out for those kinds.
	for _, k := range got.ResolvesCanonicalKinds {
		if k == "person" || k == "company" {
			t.Errorf("resolves_canonical_kinds: must not claim %q yet (BGG search reliability TBD)", k)
		}
	}

	// Per ADR-0021 + this PR: yaad-bgg emits five canonical edge
	// types — is_a (universal source-type), is_about (boardgame),
	// designed_by + artist_by (person), published_by (company).
	// Edge derivation moves from daemon-side FrontmatterEdges
	// (legacy) to plugin-emitted edges in the source-shape
	// edges block.
	wantEdgeTypes := map[string]bool{
		bgg.SourceTypeEdgeType: false,
		bgg.CanonicalEdgeType: false,
		"designed_by": false,
		"artist_by": false,
		"published_by": false,
	}
	for _, et := range got.CanonicalEdgeTypesEmitted {
		if _, ok := wantEdgeTypes[et]; ok {
			wantEdgeTypes[et] = true
		}
	}
	for et, present := range wantEdgeTypes {
		if !present {
			t.Errorf("canonical_edge_types_emitted: missing %q (got %+v)",
				et, got.CanonicalEdgeTypesEmitted)
		}
	}

	// Per ADR-0021: source_namespace declared so daemon can route
	// vault path + derive source-node entity ID.
	if got.SourceNamespace != bgg.SourceNamespace {
		t.Errorf("source_namespace: want %q, got %q",
			bgg.SourceNamespace, got.SourceNamespace)
	}

	// Per ADR-0019 step 8: declare the five operator-strategy gaps
	// the boardgame canonical kind needs. All five carry
	// fill_strategy=operator; rating is int with range [1, 10] +
	// the four others are bool.
	bgExtras, ok := got.CanonicalKindsExtras[bgg.CanonicalKind]
	if !ok {
		t.Fatalf("canonical_kinds_extras: missing %q entry", bgg.CanonicalKind)
	}
	wantGaps := map[string]gapSpecJSON{
		"rating": {
			Type: "int", Description: "How do you rate this on a 1-10 scale?",
			Range: []int{1, 10}, FillStrategy: "operator",
		},
		"owned": {Type: "bool", Description: "Do you own this?", FillStrategy: "operator"},
		"want": {Type: "bool", Description: "Do you want this?", FillStrategy: "operator"},
		"played": {Type: "bool", Description: "Have you played this?", FillStrategy: "operator"},
		"knows_how_to_play": {Type: "bool", Description: "Do you know how to play this?", FillStrategy: "operator"},
	}
	if len(bgExtras.Gaps) != len(wantGaps) {
		t.Fatalf("canonical_kinds_extras[%s].gaps: want %d entries, got %d (%+v)",
			bgg.CanonicalKind, len(wantGaps), len(bgExtras.Gaps), bgExtras.Gaps)
	}
	for field, want := range wantGaps {
		gotGap, ok := bgExtras.Gaps[field]
		if !ok {
			t.Errorf("canonical_kinds_extras.gaps: missing field %q", field)
			continue
		}
		if gotGap.Type != want.Type {
			t.Errorf("gaps[%q].type: want %q, got %q", field, want.Type, gotGap.Type)
		}
		if gotGap.Description != want.Description {
			t.Errorf("gaps[%q].description: want %q, got %q", field, want.Description, gotGap.Description)
		}
		if gotGap.FillStrategy != want.FillStrategy {
			t.Errorf("gaps[%q].fill_strategy: want %q, got %q", field, want.FillStrategy, gotGap.FillStrategy)
		}
		if !equalIntSlice(gotGap.Range, want.Range) {
			t.Errorf("gaps[%q].range: want %v, got %v", field, want.Range, gotGap.Range)
		}
	}
}

// equalIntSlice returns true when both slices share the same
// length + element order. Used by the gaps-shape assertion above —
// reflect.DeepEqual would also work but stays untyped over nil-vs-
// empty distinction; a tiny helper makes the test's intent obvious.
func equalIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRunVersion exercises the `--version` mode: yaad-index calls this
// on every startup as a cache-key probe (cheaper than a full --init
// re-handshake when the cached capabilities row's version matches).
// Output must be a bare version string + newline — matches yaad-index's
// subprocess.RunVersion parse path.
func TestRunVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	exit := run([]string{"--version"}, strings.NewReader(""), &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("--version: want exit 0, got %d (stderr=%q)", exit, stderr.String())
	}
	got := strings.TrimRight(stdout.String(), "\n")
	if got != bgg.PluginVersion {
		t.Errorf("--version: want %q, got %q", bgg.PluginVersion, got)
	}
	// No trailing junk on stdout — the parse on the index side
	// reads the whole stream, not just the first line.
	if strings.Count(stdout.String(), "\n") != 1 {
		t.Errorf("--version: want exactly one newline, got %d (stdout=%q)",
			strings.Count(stdout.String(), "\n"), stdout.String())
	}
}

// TestRunFetch_RequiresAPIKey pins the fail-closed contract per
// the operator's 2026-05-06 spec: the BGG_API_KEY env is required and
// yaad-bgg refuses to run anonymously rather than degrading silently.
//
// NOT t.Parallel — mutates process env.
func TestRunFetch_RequiresAPIKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")

	req := `{"operation":"ingest","url":"https://boardgamegeek.com/boardgame/224517"}`
	err := runFetch(context.Background(), strings.NewReader(req), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runFetch: want error when BGG_API_KEY is unset, got nil")
	}
	if !strings.Contains(err.Error(), "BGG_API_KEY") {
		t.Errorf("runFetch: want error mentioning BGG_API_KEY, got %q", err.Error())
	}
}

// TestRunFetch_RejectsMalformed exercises the request-envelope
// validation that runs BEFORE constructing the BGG client. With
// BGG_API_KEY set, malformed inputs surface with crisp errors and
// no upstream call.
//
// NOT t.Parallel — mutates process env via Setenv.
func TestRunFetch_RejectsMalformed(t *testing.T) {
	t.Setenv(EnvAPIKey, "test-key")

	cases := []struct {
		name string
		body string
		wantInErr string
	}{
		{
			name: "non_ingest_op",
			body: `{"operation":"meow","url":"https://boardgamegeek.com/boardgame/1"}`,
			wantInErr: "unsupported operation",
		},
		{
			name: "missing_url",
			body: `{"operation":"ingest"}`,
			wantInErr: "missing `url`",
		},
		{
			name: "malformed_json",
			body: `{not json`,
			wantInErr: "parse request",
		},
		{
			name: "non_bgg_url",
			body: `{"operation":"ingest","url":"https://example.com/games/1"}`,
			wantInErr: "not a recognised",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runFetch(context.Background(), strings.NewReader(tc.body), io.Discard, io.Discard)
			if err == nil {
				t.Fatalf("runFetch(%s): want error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("runFetch(%s): want error containing %q, got %q",
					tc.name, tc.wantInErr, err.Error())
			}
		})
	}
}

// TestWriteFetchResponse_NDJSONShape pins the ADR-0023 wire shape
// for runFetch's response emission: single-line JSON terminated by
// exactly one `\n`. yaad-index a prior PR's daemon-side reader uses
// json.Decoder so it parses both pretty-printed AND single-line
// shapes today; this test pins the post-migration shape so the
// future N-line consumer (yaad-index) sees the canonical wire
// format.
//
// The test exercises writeFetchResponse directly rather than driving
// the full runFetch path, since the BGG client's upstream calls
// would require a host-override plumbing that runFetch doesn't
// expose. writeFetchResponse is the single emission point both
// runFetch branches call, so pinning its shape pins the binary's
// wire format.
func TestWriteFetchResponse_NDJSONShape(t *testing.T) {
	t.Parallel()

	resp := fetchResponse{
		OK: true,
		Structured: &structuredResponse{
			Kind: "source",
			Name: "Brass: Birmingham",
			Data: map[string]any{"bgg_id": "224517"},
		},
		RawContent: "# Brass: Birmingham\n\nA description.",
	}

	var buf bytes.Buffer
	if err := writeFetchResponse(&buf, resp); err != nil {
		t.Fatalf("writeFetchResponse: %v", err)
	}

	out := buf.Bytes()
	if len(out) == 0 {
		t.Fatalf("stdout: want non-empty")
	}
	if out[len(out)-1] != '\n' {
		t.Errorf("stdout: want trailing `\\n`; got last byte %q", out[len(out)-1])
	}
	// Exactly one newline at the very end → single-line NDJSON.
	// Pretty-printed JSON would have one newline per indent break,
	// failing this assertion. RawContent's internal `\n` characters
	// are JSON-escaped as `\n` literals inside the string value, so
	// they don't count as raw newlines on the wire.
	if got := bytes.Count(out, []byte("\n")); got != 1 {
		t.Errorf("stdout: want exactly one newline (NDJSON shape); got %d (raw=%q)", got, out)
	}

	// Round-trip the single line back through json.Unmarshal so
	// the assertion isn't just a shape check — the bytes are still
	// a valid envelope.
	var got fetchResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode response after NDJSON-shape assertion: %v\nraw: %s", err, string(out))
	}
	if !got.OK || got.Structured == nil || got.Structured.Name != "Brass: Birmingham" {
		t.Errorf("envelope round-trip failed: ok=%v structured=%+v", got.OK, got.Structured)
	}
}
