// Per yaad-index the source issue — commit-message templates the auto-commit
// pipeline stamps onto vault writes. Each handler builds a message
// from per-operation context and hands it to vault.Writer.WriteWithCommit.

package api

import (
	"fmt"
	"sort"
	"strings"
)

// ingestCommitMessage produces the audit-log line for an ingest write.
//
// Templates :
// - new entity: ingest: <entity-id>
// - re-ingest, force-refetch: re-ingest: <entity-id> [force_refetch=true]
// - re-ingest, TTL-expired refresh: re-ingest: <entity-id> [ttl_expired]
// - re-ingest, plain refresh: re-ingest: <entity-id>
func ingestCommitMessage(entityID string, existing, forceRefetch, ttlExpired bool) string {
	if !existing {
		return "ingest: " + entityID
	}
	switch {
	case forceRefetch:
		return fmt.Sprintf("re-ingest: %s [force_refetch=true]", entityID)
	case ttlExpired:
		return fmt.Sprintf("re-ingest: %s [ttl_expired]", entityID)
	default:
		return "re-ingest: " + entityID
	}
}

// fillCommitMessage produces the audit line for a fill write.
//
// Template: fill: <entity-id> [field1, field2, ...]
//
// Field names are sorted alphabetically so the same fill set produces
// the same commit message regardless of map iteration order.
func fillCommitMessage(entityID string, fields map[string]any) string {
	if len(fields) == 0 {
		return "fill: " + entityID
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("fill: %s [%s]", entityID, strings.Join(keys, ", "))
}

// userContentCreateCommitMessage produces the audit line for a UGC
// create write per yaad-index.
//
// Template: create: <entity-id> by <author>
func userContentCreateCommitMessage(entityID, author string) string {
	if author == "" {
		return "create: " + entityID
	}
	return fmt.Sprintf("create: %s by %s", entityID, author)
}

// userContentEditCommitMessage produces the audit line for a UGC
// section-replace write per yaad-index.
//
// Template: edit: <entity-id> [section <sec-addr>] by <author>
func userContentEditCommitMessage(entityID, secAddr, author string) string {
	if author == "" {
		return fmt.Sprintf("edit: %s [section %s]", entityID, secAddr)
	}
	return fmt.Sprintf("edit: %s [section %s] by %s", entityID, secAddr, author)
}

// userContentFrontmatterEditCommitMessage produces the audit line
// for a UGC frontmatter-replace write per yaad-index. Distinct
// prefix `edit-frontmatter:` so a `git log` scan distinguishes
// frontmatter edits (data-map + canonical-edge re-derivation) from
// section-body edits.
//
// Template (with author): edit-frontmatter: <entity-id> by <author>
// Template (no author): edit-frontmatter: <entity-id>
func userContentFrontmatterEditCommitMessage(entityID, author string) string {
	if author == "" {
		return fmt.Sprintf("edit-frontmatter: %s", entityID)
	}
	return fmt.Sprintf("edit-frontmatter: %s by %s", entityID, author)
}

// entityDestroyCommitMessage is the commit prefix for the
// permanent-destroy path per ADR-0018 step 4: the operator already
// archived the entity, then explicitly DELETEd it. Distinct prefix
// from entityDeleteCommitMessage so an operator scanning git log
// can distinguish the (now-internal-only) active-path delete from
// the operator-driven destroy.
//
// Template (with author): destroy: <entity-id> [<kind>] by <author>
// Template (no author): destroy: <entity-id> [<kind>]
func entityDestroyCommitMessage(entityID, kind, author string) string {
	if author == "" {
		return fmt.Sprintf("destroy: %s [%s]", entityID, kind)
	}
	return fmt.Sprintf("destroy: %s [%s] by %s", entityID, kind, author)
}

// commentCommitMessage produces the audit line for a comment write.
//
// Templates:
// - comment with author: comment: <entity-id> by <author>
// - comment with empty author: comment: <entity-id>
func commentCommitMessage(entityID, author string) string {
	if author == "" {
		return "comment: " + entityID
	}
	return fmt.Sprintf("comment: %s by %s", entityID, author)
}

// agentAuthorRef shapes a commit author from an optional agent
// identity. Empty author returns empty (the Committer falls back to
// its own identity); a non-empty author is prefixed with `agent:` if
// it isn't already source-tagged (parallel to fill provenance shape
// `agent:bob`).
func agentAuthorRef(author string) string {
	if author == "" {
		return ""
	}
	if strings.ContainsRune(author, ':') {
		return author
	}
	return "agent:" + author
}
