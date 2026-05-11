// Package gmail is the yaad-gmail plugin's identity + capabilities
// surface plus the IMAP-driven Gmail integration: connect via
// IMAP+app-password+X-GM-LABELS, fetch un-ingested messages from
// inbox + sent folders, parse RFC-822 headers, emit source-shape
// entities + canonical edges (email + email-address + label),
// mark messages as ingested via Gmail-side label state.
//
// State lives on Gmail (in the configured `ingested_label`); no
// client-side state file, no UID persistence, no UIDVALIDITY
// tracking. Restart-safe by design — the next polling cycle re-runs
// the same search predicate and finds the same un-ingested set.
package gmail

import "github.com/yaad-index/yaad-index/internal/buildinfo"

// PluginName is the stable identifier surfaced as the capabilities
// document's `name` field.
const PluginName = "gmail"

// PluginVersion is the version string the plugin's `--version`
// handler emits. The Makefile-injected build-arg flows through
// internal/buildinfo.Version via ldflags; absent injection
// (`go test`, IDE builds) the sentinel `unknown` falls through to
// FallbackVersion via the precedence helper below.
var PluginVersion = resolvePluginVersion(buildinfo.Version)

// FallbackVersion is the hardcoded version reported when no
// build-time injection happened — keeps the daemon's cache-key
// probe well-defined on every binary regardless of whether the
// caller built with -ldflags.
const FallbackVersion = "0.2.0-dev"

// resolvePluginVersion folds the build-time injected
// `internal/buildinfo.Version` into the FallbackVersion sentinel.
// When the linker rewrote `Version` via `-X`, returns it verbatim;
// otherwise returns FallbackVersion.
func resolvePluginVersion(injected string) string {
	if injected == "" || injected == buildinfo.Unknown {
		return FallbackVersion
	}
	return injected
}

// SourceNamespace is the per-plugin vault path prefix and entity-ID
// namespace under the ADR-0021 universal `kind: source` contract.
// Every source-shape Gmail emission lands at id
// `gmail:<source-slug>` and vault path `gmail/<source-slug>.md`.
const SourceNamespace = "gmail"

// UniversalSourceKind is the wire-shape `kind` value the daemon
// uses to route source-shape emissions per ADR-0021. Constant
// across plugins; daemon storage layer routes to the per-plugin
// SourceNamespace above.
const UniversalSourceKind = "source"

// SourceTypeName is the descriptive name of yaad-gmail's
// source-type label — the target of the universal `is_a` edge.
// Daemon's slug.Slug derives the canonical-label slug
// (`source-type:gmail`).
const SourceTypeName = "gmail"

// CanonicalKindEmail / -EmailAddress / -Label are the canonical
// kinds yaad-gmail emits per the spec. `email` is the primary
// `is_about` target; `email-address` is the per-unique-address
// canonical entity that `from`/`to`/`cc`/`bcc` edges target;
// `label` is the per-unique-Gmail-label canonical entity that
// `tagged_as` edges target.
const (
	CanonicalKindEmail = "email"
	CanonicalKindEmailAddress = "email-address"
	CanonicalKindLabel = "label"
)

// KnownCanonicalKinds is the set of canonical kinds yaad-gmail
// declares it MAY emit, surfaced through `Capabilities.
// CanonicalKindsEmitted` per ADR-0008. Alphabetical for stable
// diffs.
var KnownCanonicalKinds = []string{
	CanonicalKindEmail,
	CanonicalKindEmailAddress,
	CanonicalKindLabel,
}

// CanonicalEdgeType / SourceTypeEdgeType / FromEdgeType / etc. —
// the edge-type vocabulary yaad-gmail emits. `is_about` targets
// the `email` canonical entity; `is_a` is the universal source-type
// edge per ADR-0021; `from`/`to`/`cc`/`bcc` target email-address
// canonical entities (bcc only on sent-folder messages); `tagged_as`
// targets label canonical entities.
const (
	EdgeTypeIsAbout = "is_about"
	EdgeTypeIsA = "is_a"
	EdgeTypeFrom = "from"
	EdgeTypeTo = "to"
	EdgeTypeCc = "cc"
	EdgeTypeBcc = "bcc"
	EdgeTypeTaggedAs = "tagged_as"
)

// KnownCanonicalEdgeTypes is the alphabetically-sorted set of edge
// types yaad-gmail declares — surfaced via
// `Capabilities.CanonicalEdgeTypesEmitted`.
var KnownCanonicalEdgeTypes = []string{
	EdgeTypeBcc,
	EdgeTypeCc,
	EdgeTypeFrom,
	EdgeTypeIsA,
	EdgeTypeIsAbout,
	EdgeTypeTaggedAs,
	EdgeTypeTo,
}

// Default values for the plugin's operator-controlled config knobs
// (per the spec's Configuration section). Empty-string-disables on
// IngestedLabel + SkipLabel is enforced at use time (poll loop
// + label-flow filter).
const (
	DefaultPollingInterval = "5m"
	DefaultIngestedLabel = "alice2-ingested"
	DefaultSkipLabel = "alice2-skip"
	DefaultIMAPHost = "imap.gmail.com"
	DefaultIMAPPort = 993
)

// CommandFetch is the named command this plugin handles per
// ADR-0022 §1. Bare name, no `!` sigil; the operator-side
// invocation surface (`gmail: !fetch`) supplies the sigil.
const CommandFetch = "fetch"

// DeclaredCommands is the list yaad-gmail surfaces in its --init
// Capabilities document and uses to validate `--command` flag
// values. Today the only declared command is `fetch`.
var DeclaredCommands = []string{CommandFetch}

// SentFolderName is the IMAP folder name Gmail uses for sent
// messages. Distinct from `INBOX` because BCC parsing is only
// applied on messages from the sent folder (inbound mail never
// carries visible BCC headers).
const SentFolderName = "[Gmail]/Sent Mail"

// InboxFolderName is the IMAP folder name for received messages.
const InboxFolderName = "INBOX"
