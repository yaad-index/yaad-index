package gmail

import "strings"

// Edge is one outgoing edge on a source-shape Gmail entity. Mirrors
// the canonical-edge wire shape the daemon's source emission path
// expects per ADR-0021: `{type, name, kind}` where the daemon
// derives the canonical-label endpoint as `<kind>:<slug.Slug(name)>`.
//
// yaad-gmail emits seven edge types per the spec:
// - is_about → email
// - is_a → source-type:gmail
// - from → email-address
// - to → email-address (one per recipient)
// - cc → email-address (one per recipient)
// - bcc → email-address (sent-folder only, one per recipient)
// - tagged_as → label (one per surfaced Gmail label)
type Edge struct {
	// Type is the edge type — one of the EdgeType* constants.
	Type string
	// Name is the descriptive endpoint name; daemon slugifies for
	// the canonical-label id. Address slugs and label slugs use
	// the per-kind encoders (EmailAddressSlug, LabelSlug); the
	// assembler computes those locally and supplies the slugified
	// name as Name so the daemon's slug pass is a no-op (the
	// pre-slugified name passes through unchanged).
	Name string
	// Kind is the canonical kind the daemon resolves Name through.
	Kind string
}

// AssembleEdges turns a ParsedMessage into the canonical edge list
// the daemon will materialize. Filter rules:
//
// - `tagged_as` edges exclude `ingestedLabel` and `skipLabel`
// (control-plane labels — present on Gmail-side, not surfaced
// in the entity graph).
// - `bcc` edges are emitted only when ParsedMessage.IsSentFolder
// is true (inbound BCC headers don't reach the recipient
// reliably; the spec scopes BCC to sent-folder messages).
// - `from` is single-valued (RFC-5322 multi-sender headers
// collapse to the first address by ParseMessage's contract).
//
// The assembler does NOT emit the `is_a` source-type edge or any
// internal source-shape book-keeping — those land in the daemon
// emission envelope at the wire layer. AssembleEdges' output is
// the cross-canonical edge set ONLY (is_about + from/to/cc/bcc +
// tagged_as).
func AssembleEdges(pm *ParsedMessage, ingestedLabel, skipLabel string) []Edge {
	if pm == nil {
		return nil
	}

	// Capacity hint: 1 (is_about) + 1 (from) + len(to) + len(cc)
	// + len(bcc) + len(labels). Over-allocates slightly in the
	// excluded-label / no-from cases; harmless.
	capHint := 2 + len(pm.To) + len(pm.Cc) + len(pm.Bcc) + len(pm.Labels)
	out := make([]Edge, 0, capHint)

	if pm.MessageID != "" {
		out = append(out, Edge{
			Type: EdgeTypeIsAbout,
			Name: EmailCanonicalSlug(pm.MessageID),
			Kind: CanonicalKindEmail,
		})
	}

	if pm.From != "" {
		out = append(out, Edge{
			Type: EdgeTypeFrom,
			Name: EmailAddressSlug(pm.From),
			Kind: CanonicalKindEmailAddress,
		})
	}
	for _, addr := range pm.To {
		out = append(out, Edge{
			Type: EdgeTypeTo,
			Name: EmailAddressSlug(addr),
			Kind: CanonicalKindEmailAddress,
		})
	}
	for _, addr := range pm.Cc {
		out = append(out, Edge{
			Type: EdgeTypeCc,
			Name: EmailAddressSlug(addr),
			Kind: CanonicalKindEmailAddress,
		})
	}
	if pm.IsSentFolder {
		for _, addr := range pm.Bcc {
			out = append(out, Edge{
				Type: EdgeTypeBcc,
				Name: EmailAddressSlug(addr),
				Kind: CanonicalKindEmailAddress,
			})
		}
	}

	for _, label := range pm.Labels {
		if label == "" {
			continue
		}
		if label == ingestedLabel || label == skipLabel {
			continue
		}
		if isSystemLabel(label) {
			// Gmail system labels (the X-GM-LABELS special-use flags
			// `\Inbox`, `\Sent`, `\Unread`, `\Important`, `\Starred`,
			// `\Draft`, `\Spam`, `\Trash`, …) are not semantic
			// operator tags — emitting tagged_as edges for them
			// pollutes the graph with non-semantic relationships
			// (#449). Only the two control-plane labels were filtered
			// before.
			continue
		}
		out = append(out, Edge{
			Type: EdgeTypeTaggedAs,
			Name: LabelSlug(label),
			Kind: CanonicalKindLabel,
		})
	}

	return out
}

// isSystemLabel reports whether a Gmail label is a system / special-use
// flag rather than an operator-applied label. The X-GM-LABELS extension
// prefixes system flags with a backslash (`\Inbox`, `\Sent`, `\Unread`,
// `\Important`, `\Starred`, `\Draft`, `\Spam`, `\Trash`); operator
// labels carry no such prefix.
func isSystemLabel(label string) bool {
	return strings.HasPrefix(label, `\`)
}
