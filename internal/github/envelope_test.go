package github

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleItem(kind ItemKind) *Item {
	updated := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	closed := time.Date(2026, 5, 20, 12, 30, 0, 0, time.UTC)
	merged := time.Date(2026, 5, 20, 12, 31, 0, 0, time.UTC)
	return &Item{
		Target:        Target{Owner: "acme", Repo: "proj", Kind: kind, Number: 42},
		Number:        42,
		Type:          kind,
		State:         "closed",
		Title:         "Sample title",
		Body:          "Body markdown.",
		URL:           "https://github.com/acme/proj/pull/42",
		CreatedAt:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt:     updated,
		ClosedAt:      &closed,
		MergedAt:      &merged,
		Merged:        kind == ItemKindPR,
		Author:        "author-user",
		Assignees:     []string{"assignee-a"},
		Reviewers:     []string{"reviewer-a", "reviewer-b"},
		CommentCount:  5,
		LastCommentAt: &updated,
		BaseBranch:    "main",
		HeadBranch:    "feat/p",
		Labels:        []string{"bug", "p1"},
	}
}

func decodeEnvelope(t *testing.T, buf *bytes.Buffer) envelopeDoc {
	t.Helper()
	var doc envelopeDoc
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc), "envelope must be valid JSON: %s", buf.String())
	return doc
}

func TestWriteEnvelope_PR_ShapeMatchesADR(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, sampleItem(ItemKindPR),
		"",
		"",
		"https://github.com/acme/proj/pull/42",
		"2026-05-20T12:00:00Z"))

	// NDJSON contract: one line + trailing newline.
	assert.True(t, strings.HasSuffix(buf.String(), "\n"))
	assert.Equal(t, 1, strings.Count(buf.String(), "\n"), "single-line NDJSON envelope")

	doc := decodeEnvelope(t, &buf)
	assert.True(t, doc.OK)
	require.NotNil(t, doc.Structured)

	s := doc.Structured
	assert.Equal(t, "source", s.Kind, "ADR-0021 universal source kind")
	assert.Equal(t, "acme_proj_pr_42", s.Name, "ADR-0026 §2 slug-target shape")

	// Data: required + PR-specific fields.
	d := s.Data
	assert.Equal(t, float64(42), d["number"])
	assert.Equal(t, "pr", d["type"])
	assert.Equal(t, "closed", d["state"])
	assert.Equal(t, "Sample title", d["title"])
	assert.Equal(t, "https://github.com/acme/proj/pull/42", d["url"])
	assert.Equal(t, "author-user", d["author"])
	assert.Equal(t, float64(5), d["comment_count"])
	assert.Equal(t, true, d["merged"])
	assert.Equal(t, "main", d["base_branch"])
	assert.Equal(t, "feat/p", d["head_branch"])

	// Edges: ADR-0026 §1 + §Consequences.
	assert.Equal(t, []edgeTargetDoc{{Name: "github-record", Kind: "source-type"}}, s.Edges["is_a"])
	assert.Equal(t, []edgeTargetDoc{{Name: "acme/proj", Kind: "repository"}}, s.Edges["in_repo"])
	assert.Equal(t, []edgeTargetDoc{{Name: "author-user", Kind: "github-user"}}, s.Edges["authored_by"])
	assert.Equal(t, []edgeTargetDoc{
		{Name: "assignee-a", Kind: "github-user"},
		{Name: "author-user", Kind: "github-user"},
		{Name: "reviewer-a", Kind: "github-user"},
		{Name: "reviewer-b", Kind: "github-user"},
	}, s.Edges["involves"], "involves dedupes author + assignees + reviewers, sorted")
	assert.Equal(t, []edgeTargetDoc{{Name: "assignee-a", Kind: "github-user"}}, s.Edges["assigned_to"])
	assert.Equal(t, []edgeTargetDoc{
		{Name: "reviewer-a", Kind: "github-user"},
		{Name: "reviewer-b", Kind: "github-user"},
	}, s.Edges["reviewed_by"], "PR-only edge")

	// Body lands in raw_content per ADR-0015.
	assert.Equal(t, "Body markdown.", doc.RawContent)

	// Notations: originating input first per ADR-0021 self-
	// roundtrip invariant; remaining canonical forms follow,
	// deduped.
	require.Len(t, doc.Notations, 2)
	assert.Equal(t, "https://github.com/acme/proj/pull/42", doc.Notations[0])
	assert.Equal(t, "github:acme/proj#42", doc.Notations[1])

	// Per-fetch cache TTL override mirrors the plugin-level
	// default. Pointer-shape on the wire (matches yaad-bgg /
	// yaad-wikipedia pattern).
	require.NotNil(t, doc.CacheTTLSeconds)
	assert.Equal(t, DefaultCacheTTLSeconds, *doc.CacheTTLSeconds)

	// Provenance: one entry stamped by the plugin.
	require.Len(t, s.Provenance, 1)
	assert.Equal(t, "github", s.Provenance[0].Source)
	assert.Equal(t, "2026-05-20T12:00:00Z", s.Provenance[0].FetchedAt)
	assert.True(t, s.Provenance[0].OK)
}

func TestWriteEnvelope_Issue_NoPRSpecificFields(t *testing.T) {
	t.Parallel()
	item := sampleItem(ItemKindIssue)
	item.URL = "https://github.com/acme/proj/issues/42"
	item.Merged = false
	item.MergedAt = nil
	item.Reviewers = nil
	item.BaseBranch = ""
	item.HeadBranch = ""

	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, item, "", "", "github:acme/proj#42", "2026-05-20T12:00:00Z"))
	doc := decodeEnvelope(t, &buf)
	require.NotNil(t, doc.Structured)

	s := doc.Structured
	assert.Equal(t, "acme_proj_issue_42", s.Name)
	d := s.Data
	assert.Equal(t, "issue", d["type"])
	assert.NotContains(t, d, "merged", "issue envelope must not carry PR-only merged flag")
	assert.NotContains(t, d, "merged_at")
	assert.NotContains(t, d, "base_branch")
	assert.NotContains(t, d, "head_branch")
	// reviewed_by edge bucket should be absent for issues.
	_, hasReviewedBy := s.Edges["reviewed_by"]
	assert.False(t, hasReviewedBy, "reviewed_by must be PR-only")
}

func TestWriteEnvelope_OriginatingNotation_FirstWhenShorthand(t *testing.T) {
	t.Parallel()
	item := sampleItem(ItemKindPR)
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, item, "", "", "github:acme/proj#42", "t"))
	doc := decodeEnvelope(t, &buf)
	require.Len(t, doc.Notations, 2)
	assert.Equal(t, "github:acme/proj#42", doc.Notations[0], "shorthand originating input leads")
	assert.Equal(t, "https://github.com/acme/proj/pull/42", doc.Notations[1])
}

func TestWriteEnvelope_NoBody_NoRawContentField(t *testing.T) {
	t.Parallel()
	item := sampleItem(ItemKindIssue)
	item.Body = ""
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, item, "", "", "x", "t"))

	// Envelope JSON should omit `raw_content` when empty
	// (omitempty tag on the wire shape).
	var raw map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &raw))
	_, has := raw["raw_content"]
	assert.False(t, has, "raw_content omitted when body empty")
}

func TestWriteEnvelope_NilItem_Errors(t *testing.T) {
	t.Parallel()
	err := WriteEnvelope(&bytes.Buffer{}, nil, "", "", "x", "t")
	require.Error(t, err)
}

func TestWriteEnvelope_NotationsDedupe_WhenOriginatingMatchesCanonical(t *testing.T) {
	t.Parallel()
	// If the originating input IS the canonical URL form,
	// the notations list shouldn't carry duplicates.
	item := sampleItem(ItemKindPR)
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, item, "", "", "https://github.com/acme/proj/pull/42", "t"))
	doc := decodeEnvelope(t, &buf)
	require.Len(t, doc.Notations, 2, "originating + shorthand only (canonical URL deduped)")
	assert.Equal(t, "https://github.com/acme/proj/pull/42", doc.Notations[0])
	assert.Equal(t, "github:acme/proj#42", doc.Notations[1])
}

func TestWriteEnvelope_InstanceName_ThreadsIntoShorthand(t *testing.T) {
	t.Parallel()
	// ADR-0026 §7 multi-instance: a GHES instance running
	// under the operator-side name `github-work` should emit
	// shorthand `github-work:owner/repo#N`, not the bare
	// `github:owner/repo#N`. Otherwise a same-input re-ingest
	// misses the entity_notations cache hit on the shorthand
	// (URL hit still works fine, but defeats the cache
	// pre-registration for shorthand-initiated calls).
	item := sampleItem(ItemKindPR)
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, item,
		"github-work",                          // instance name
		"https://ghes.example.com/api/v3",      // base URL
		"https://github.com/acme/proj/pull/42", // originating URL
		"2026-05-20T12:00:00Z"))
	doc := decodeEnvelope(t, &buf)
	require.Len(t, doc.Notations, 2)
	assert.Equal(t, "https://github.com/acme/proj/pull/42", doc.Notations[0])
	assert.Equal(t, "github-work:acme/proj#42", doc.Notations[1],
		"shorthand must use instance name `github-work`, not bare `github`")
}

func TestWriteEnvelope_EmptyInstanceName_FallsBackToPluginName(t *testing.T) {
	t.Parallel()
	// Empty instance name (single-instance / test default)
	// must produce the canonical `github:` shorthand —
	// mirrors BuildURLPatterns's same fallback in github.go.
	item := sampleItem(ItemKindPR)
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, item, "", "", "x", "t"))
	doc := decodeEnvelope(t, &buf)
	require.GreaterOrEqual(t, len(doc.Notations), 2)
	// Find the shorthand in the list (its position depends on
	// the originating-input value; just confirm it's there
	// with the expected prefix).
	hasGithubShorthand := false
	for _, n := range doc.Notations {
		if strings.HasPrefix(n, "github:") {
			hasGithubShorthand = true
			break
		}
	}
	assert.True(t, hasGithubShorthand, "empty instance name must fall back to `github:` shorthand")
}

func TestWriteEnvelope_GHESBaseURL_SynthesizesGHESHostInCanonicalURLFallback(t *testing.T) {
	t.Parallel()
	// Defensive-fallback path: when item.URL is empty (upstream
	// didn't populate html_url), buildNotations synthesizes the
	// canonical URL from owner/repo/kind/num. Pre-fix the host
	// hardcoded to `github.com`; for a GHES instance that's
	// wrong (operator's GHES URLs live under `ghes.example.com`
	// or similar). With the fix, the synthesizer uses the host
	// from the configured base URL.
	item := sampleItem(ItemKindPR)
	item.URL = "" // force the defensive path
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, item,
		"github-work",
		"https://ghes.example.com/api/v3",
		"github-work:acme/proj#42",
		"t"))
	doc := decodeEnvelope(t, &buf)
	// Expect the synthesized canonical URL to use the GHES
	// host, not github.com.
	hasGHES := false
	hasGithubCom := false
	for _, n := range doc.Notations {
		if strings.Contains(n, "ghes.example.com") {
			hasGHES = true
		}
		if strings.Contains(n, "github.com") {
			hasGithubCom = true
		}
	}
	assert.True(t, hasGHES, "GHES instance canonical-URL fallback must use the GHES host: %v", doc.Notations)
	assert.False(t, hasGithubCom, "GHES instance must NOT synthesize public github.com URLs in the fallback: %v", doc.Notations)
}

func TestWriteEnvelope_EmptyBaseURL_FallbackUsesGithubCom(t *testing.T) {
	t.Parallel()
	// Empty base URL + empty item.URL → fallback to the public
	// github.com host. Mirrors the public-default behavior at
	// the resolver layer.
	item := sampleItem(ItemKindIssue)
	item.URL = ""
	item.Type = ItemKindIssue
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, item, "", "", "x", "t"))
	doc := decodeEnvelope(t, &buf)
	hasGithubCom := false
	for _, n := range doc.Notations {
		if strings.HasPrefix(n, "https://github.com/") && strings.Contains(n, "/issues/") {
			hasGithubCom = true
			break
		}
	}
	assert.True(t, hasGithubCom, "empty base URL fallback must use github.com: %v", doc.Notations)
}

func TestBuildData_SortsLabels(t *testing.T) {
	t.Parallel()
	item := sampleItem(ItemKindIssue)
	item.Labels = []string{"zzz", "aaa", "mmm"}
	data := buildData(item)
	labels, ok := data["labels"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"aaa", "mmm", "zzz"}, labels)
}
