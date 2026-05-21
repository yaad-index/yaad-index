package github

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// envelopeDoc mirrors the wire shape yaad-index's
// subprocess.fetchResponse / sourceLine decodes per ADR-0023.
// One JSON object per ingest emission; the field shape
// matches what yaad-bgg / yaad-wikipedia / yaad-gmail emit.
//
// `RawContent` is the markdown body the daemon wraps between
// `<!-- yaad:plugin start/end -->` markers per ADR-0015. We
// stream it through verbatim; the daemon owns the markering.
type envelopeDoc struct {
	OK              bool               `json:"ok"`
	Structured      *structuredEnvelope `json:"structured,omitempty"`
	RawContent      string             `json:"raw_content,omitempty"`
	Notations       []string           `json:"notations,omitempty"`
	Aliases         []string           `json:"aliases,omitempty"`
	CacheTTLSeconds *int               `json:"cache_ttl_seconds,omitempty"`
}

// structuredEnvelope mirrors yaad-index's
// subprocess.structuredResponse under the ADR-0021
// universal-source-shape contract. `kind: "source"` +
// descriptive `name` + `data` + `edges` map-keyed-by-type.
type structuredEnvelope struct {
	Kind       string                       `json:"kind"`
	Name       string                       `json:"name,omitempty"`
	Data       map[string]any               `json:"data,omitempty"`
	Edges      map[string][]edgeTargetDoc   `json:"edges,omitempty"`
	Provenance []provenanceEntryDoc         `json:"provenance,omitempty"`
}

// edgeTargetDoc is one `{name, kind}` descriptive ref the
// daemon resolves to a canonical-label edge via
// canonical.EnsureLabelRow.
type edgeTargetDoc struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type provenanceEntryDoc struct {
	Source    string `json:"source"`
	FetchedAt string `json:"fetched_at,omitempty"`
	OK        bool   `json:"ok"`
}

// WriteEnvelope serializes an Item into an NDJSON line on w
// per ADR-0023 (one JSON envelope per line, trailing `\n`).
// Suitable for both the URL-shape single-emission path (Cut
// 2) and the command-shape bulk path (Cut 3 — same shape,
// just emitted N times).
//
// `originatingInput` is the literal string the caller fed to
// ParseTarget; it lands first in `notations[]` per ADR-0021's
// self-roundtrip-first invariant so the daemon's
// entity_notations cache hit on a re-ingest.
//
// `fetchedAt` is the RFC-3339 stamp the plugin records on
// the single emitted provenance entry. The caller threads
// `time.Now().Format(time.RFC3339)` (or a TZ-aware variant
// per YAAD_TIMEZONE if the plugin's env-var honors it) in.
func WriteEnvelope(w io.Writer, item *Item, originatingInput, fetchedAt string) error {
	if item == nil {
		return fmt.Errorf("github: WriteEnvelope called with nil item")
	}
	doc := envelopeDoc{
		OK:              true,
		Structured:      buildStructured(item, fetchedAt),
		RawContent:      item.Body,
		Notations:       buildNotations(item, originatingInput),
		Aliases:         nil,
		CacheTTLSeconds: cacheTTLPtr(DefaultCacheTTLSeconds),
	}
	enc := json.NewEncoder(w)
	// ADR-0023: single-line JSON + trailing `\n`. json.Encoder
	// already appends `\n`; no SetIndent (would break the
	// NDJSON contract).
	return enc.Encode(doc)
}

func buildStructured(item *Item, fetchedAt string) *structuredEnvelope {
	return &structuredEnvelope{
		Kind:  UniversalSourceKind,
		Name:  item.Target.EntityName(),
		Data:  buildData(item),
		Edges: buildEdges(item),
		Provenance: []provenanceEntryDoc{
			{Source: PluginName, FetchedAt: fetchedAt, OK: true},
		},
	}
}

// buildData composes the `structured.data` map per ADR-0026's
// "Entity shape" section in issue #187. PR-specific +
// issue-specific fields are gated behind item.IsPR() / labels
// length to keep null fields out of the wire shape — operators
// reading vault YAML see only the fields the source actually
// produces.
func buildData(item *Item) map[string]any {
	data := map[string]any{
		"number":        item.Number,
		"type":          string(item.Type),
		"state":         item.State,
		"title":         item.Title,
		"url":           item.URL,
		"comment_count": item.CommentCount,
	}
	if item.Author != "" {
		data["author"] = item.Author
	}
	if !item.CreatedAt.IsZero() {
		data["created_at"] = item.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if !item.UpdatedAt.IsZero() {
		data["updated_at"] = item.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if item.ClosedAt != nil {
		data["closed_at"] = item.ClosedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if item.LastCommentAt != nil {
		data["last_comment_at"] = item.LastCommentAt.UTC().Format("2006-01-02T15:04:05Z")
	}

	if item.IsPR() {
		data["merged"] = item.Merged
		if item.MergedAt != nil {
			data["merged_at"] = item.MergedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if item.BaseBranch != "" {
			data["base_branch"] = item.BaseBranch
		}
		if item.HeadBranch != "" {
			data["head_branch"] = item.HeadBranch
		}
	}

	if len(item.Labels) > 0 {
		labels := append([]string(nil), item.Labels...)
		sort.Strings(labels)
		data["labels"] = labels
	}
	return data
}

// buildEdges composes the `structured.edges` block per ADR-
// 0026 §1 + §Consequences. Each value is a list of `{name,
// kind}` descriptive refs the daemon resolves to canonical-
// label edges. Empty edge-type buckets are omitted so the
// wire shape stays tight.
func buildEdges(item *Item) map[string][]edgeTargetDoc {
	edges := map[string][]edgeTargetDoc{
		EdgeTypeIsA: {{Name: SourceTypeName, Kind: SourceTypeKind}},
		EdgeTypeInRepo: {{
			Name: fmt.Sprintf("%s/%s", item.Target.Owner, item.Target.Repo),
			Kind: CanonicalKindRepository,
		}},
	}
	if item.Author != "" {
		edges[EdgeTypeAuthoredBy] = []edgeTargetDoc{
			{Name: item.Author, Kind: CanonicalKindUser},
		}
	}

	// `involves` is the broad GitHub-side scope per ADR-0026
	// §4: author + assignees + reviewers + commenters +
	// mentioned-in. Cut 2 (single-item fetch) covers the
	// authoritative subset we have on the response (author,
	// assignees, reviewers); commenters + mentioned-in would
	// require an extra timeline-pagination round-trip that's
	// out of scope for v1. The bulk fetch path (Cut 3) uses
	// GitHub Search's `involves:<login>` filter for membership
	// signal at the source rather than reconstructing it from
	// the per-item shape.
	involvesSet := map[string]struct{}{}
	addLogin := func(login string) {
		if login == "" {
			return
		}
		involvesSet[login] = struct{}{}
	}
	addLogin(item.Author)
	for _, a := range item.Assignees {
		addLogin(a)
	}
	for _, r := range item.Reviewers {
		addLogin(r)
	}
	if len(involvesSet) > 0 {
		logins := make([]string, 0, len(involvesSet))
		for l := range involvesSet {
			logins = append(logins, l)
		}
		sort.Strings(logins)
		bucket := make([]edgeTargetDoc, 0, len(logins))
		for _, l := range logins {
			bucket = append(bucket, edgeTargetDoc{Name: l, Kind: CanonicalKindUser})
		}
		edges[EdgeTypeInvolves] = bucket
	}

	if len(item.Assignees) > 0 {
		bucket := make([]edgeTargetDoc, 0, len(item.Assignees))
		for _, a := range item.Assignees {
			bucket = append(bucket, edgeTargetDoc{Name: a, Kind: CanonicalKindUser})
		}
		edges[EdgeTypeAssignedTo] = bucket
	}
	if item.IsPR() && len(item.Reviewers) > 0 {
		bucket := make([]edgeTargetDoc, 0, len(item.Reviewers))
		for _, r := range item.Reviewers {
			bucket = append(bucket, edgeTargetDoc{Name: r, Kind: CanonicalKindUser})
		}
		edges[EdgeTypeReviewedBy] = bucket
	}
	return edges
}

// buildNotations composes the `notations` list per ADR-0021's
// self-roundtrip-first invariant: the originating input the
// caller passed comes first so the daemon's entity_notations
// cache hit on a same-input re-ingest. The remaining derived
// forms (canonical URL + the canonical shorthand) follow,
// deduped against the originating notation.
func buildNotations(item *Item, originatingInput string) []string {
	canonicalURL := item.URL
	if canonicalURL == "" {
		// Fallback: synthesize from the target. The PR/issue
		// fetch path populates HTMLURL in the happy path; this
		// branch exists for defensive completeness.
		path := "pull"
		if item.Type == ItemKindIssue {
			path = "issues"
		}
		canonicalURL = fmt.Sprintf("https://github.com/%s/%s/%s/%d",
			item.Target.Owner, item.Target.Repo, path, item.Number)
	}
	shorthand := fmt.Sprintf("%s:%s/%s#%d",
		PluginName, item.Target.Owner, item.Target.Repo, item.Number)

	notations := []string{}
	seen := map[string]struct{}{}
	add := func(n string) {
		if n == "" {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		notations = append(notations, n)
	}
	add(originatingInput)
	add(canonicalURL)
	add(shorthand)
	return notations
}

func cacheTTLPtr(seconds int) *int {
	return &seconds
}
