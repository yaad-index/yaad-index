// Command yaad-gmail is the standalone Gmail extractor binary for
// yaad-index, implementing the subprocess plugin protocol from
// ADR-0006.
//
// CLI modes (per ADR-0006 + ADR-0022):
//
// - `yaad-gmail --init` — write the capabilities document
// (name, version, source kind, canonical kinds + edge types,
// commands list) as JSON to stdout and exit 0.
//
// - `yaad-gmail --version` — write the bare version string to
// stdout and exit 0. Answers yaad-index's startup cache-key
// probe without the full --init handshake.
//
// - `yaad-gmail --command fetch` (per ADR-0022 §1) — runs ONE
// IMAP poll cycle and emits NDJSON source-emission envelopes
// per ADR-0023 (one JSON line per un-ingested message,
// terminated with `\n`, plus a final `_summary` packet with
// {ingested, errors, duration_ms}). Per-message commit on
// emit success: the `yaad-ingested` Gmail label flips
// immediately so the next cycle's IMAP-side search excludes
// the message. Auth (account_email + app_password) flows in
// via env vars: YAAD_GMAIL_ACCOUNT + YAAD_GMAIL_APP_PASSWORD.
// Label slots default to yaad-ingested + yaad-skip; override
// via env.
//
// - `yaad-gmail` (no args) — convenience alias for
// `--command fetch` so manual operator invocation doesn't
// need to remember the flag. Same NDJSON wire shape.
//
// Per-envelope binary attachments (MIME parts) are deferred to a
// follow-up issue (yaad-index); v1 emits cleanContent only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/gmail"
)

// Env-var integration points for operator config (per ADR-0006:
// yaad-index inherits env into the subprocess; the config
// allowlist takes a path only, not args). Mirrors yaad-wikipedia's
// env-passthrough pattern.
const (
	EnvAccountEmail = "YAAD_GMAIL_ACCOUNT"
	EnvAppPassword = "YAAD_GMAIL_APP_PASSWORD"
	EnvIngestedLabel = "YAAD_GMAIL_INGESTED_LABEL"
	EnvSkipLabel = "YAAD_GMAIL_SKIP_LABEL"
	EnvPollingInterval = "YAAD_GMAIL_POLLING_INTERVAL"
	EnvIMAPHost = "YAAD_GMAIL_IMAP_HOST"
	EnvIMAPPort = "YAAD_GMAIL_IMAP_PORT"
)

// requestTimeout caps the wall-clock budget for one ingest
// invocation (one poll cycle, including IMAP connect / search /
// fetch / store). Long enough to handle a few hundred un-ingested
// messages; short enough that a stuck IMAP connection doesn't hang
// the binary forever.
const requestTimeout = 5 * time.Minute

func main() {
	exit := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(exit)
}

// run is the testable entry point. Returns the exit code rather
// than calling os.Exit so tests can drive runInit / runVersion /
// runCommandFetch directly with a buffer pair. Exit codes: 0
// success, 1 runtime error, 2 bad flags.
func run(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("yaad-gmail", flag.ContinueOnError)
	fs.SetOutput(stderr)
	initMode := fs.Bool("init", false,
		"emit the capabilities document on stdout and exit (called by yaad-index at startup)")
	versionMode := fs.Bool("version", false,
		"print the plugin version and exit (called by yaad-index's cache-key probe)")
	commandName := fs.String("command", "",
		"named command to run (per ADR-0022 §1). Today the only declared command is `fetch` — runs one IMAP poll cycle, emits NDJSON envelopes.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *versionMode {
		_, _ = fmt.Fprintln(stdout, gmail.PluginVersion)
		return 0
	}

	if *initMode {
		if err := runInit(stdout); err != nil {
			_, _ = fmt.Fprintf(stderr, "yaad-gmail --init: %v\n", err)
			return 1
		}
		return 0
	}

	// Resolve the named command. Empty `--command` + no other flag
	// is treated as the convenience alias for `--command fetch`
	// (matches the pre-ADR-0022 default-mode shape but with the
	// post-ADR-0023 NDJSON wire format).
	cmd := *commandName
	if cmd == "" {
		cmd = "fetch"
	}

	switch cmd {
	case "fetch":
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		if err := runCommandFetch(ctx, stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "yaad-gmail --command fetch: %v\n", err)
			return 1
		}
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "yaad-gmail: unknown --command %q (declared: %v)\n",
			cmd, gmail.DeclaredCommands)
		return 2
	}
}

// --- capabilities (--init) ---

type capabilitiesDoc struct {
	Name string `json:"name"`
	Version string `json:"version"`
	URLPatterns []string `json:"url_patterns"`
	EntityKinds []kindSpecJSON `json:"entity_kinds"`
	EdgeKinds []kindSpecJSON `json:"edge_kinds"`
	CanonicalKindsEmitted []string `json:"canonical_kinds_emitted,omitempty"`
	CanonicalEdgeTypesEmitted []string `json:"canonical_edge_types_emitted,omitempty"`
	SourceNamespace string `json:"source_namespace,omitempty"`
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
	// Commands declares the named imperative invocations this plugin
	// exposes per ADR-0022 §1. Bare names, no `!` sigil — the sigil
	// lives only in the operator-side invocation surface
	// (`gmail: !fetch`). yaad-gmail's only command today is `fetch`,
	// which runs one IMAP poll cycle.
	Commands []string `json:"commands,omitempty"`
}

type kindSpecJSON struct {
	Name string `json:"name"`
	DefaultTTLDays int `json:"default_ttl_days,omitempty"`
}

// runInit emits the capabilities document. Post- shape: declares
// the `gmail` source-shape kind, the three canonical kinds (email,
// email-address, label) yaad-gmail emits, and the seven edge types
// (is_about, is_a, from, to, cc, bcc, tagged_as). URL patterns
// stay empty — Gmail messages don't have a URL form yaad-index
// dispatches against; this plugin is poll-driven, not URL-driven.
func runInit(stdout io.Writer) error {
	doc := capabilitiesDoc{
		Name: gmail.PluginName,
		Version: gmail.PluginVersion,
		// Gmail has no URL form — operator can't `/v1/ingest` a
		// gmail message by URL. Polling drives all ingest. Empty
		// patterns means yaad-index never dispatches a /v1/ingest
		// to this plugin via URL match; the binary's default mode
		// runs a poll cycle when invoked.
		URLPatterns: []string{},
		EntityKinds: []kindSpecJSON{
			{Name: gmail.UniversalSourceKind},
		},
		EdgeKinds: []kindSpecJSON{},
		CanonicalKindsEmitted: gmail.KnownCanonicalKinds,
		CanonicalEdgeTypesEmitted: gmail.KnownCanonicalEdgeTypes,
		SourceNamespace: gmail.SourceNamespace,
		Commands: gmail.DeclaredCommands,
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", " ")
	return enc.Encode(doc)
}

// --- ingest (--command fetch, per ADR-0022 §1 + ADR-0023) ---

// sourceLine is the per-message NDJSON wire shape yaad-gmail emits
// during `--command fetch`. Mirrors yaad-wikipedia + yaad-bgg's
// post-migration single-line shape: top-level `ok` + ADR-0021
// `structured` block. Each invocation of `--command fetch` emits
// zero or more of these (one per un-ingested message), terminated
// by a trailing `\n`, optionally followed by a `_summary` packet.
type sourceLine struct {
	OK bool `json:"ok"`
	Structured *structuredEnvelope `json:"structured,omitempty"`
}

// structuredEnvelope is the per-message ADR-0021 structured payload.
// Daemon derives the entity ID via `<source_namespace>:<slug.Slug
// (name)>`; yaad-gmail emits `name` as the already-slugged form
// (gmail.SourceSlug result) so daemon's slugify is idempotent.
type structuredEnvelope struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Data map[string]any `json:"data,omitempty"`
	Edges map[string][]edgeTargetJSON `json:"edges,omitempty"`
	Provenance []provenanceJSONEntry `json:"provenance,omitempty"`
}

// edgeTargetJSON is one descriptive `{name, kind}` reference per
// ADR-0021's polymorphic edges block.
type edgeTargetJSON struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type provenanceJSONEntry struct {
	Source string `json:"source"`
	FetchedAt string `json:"fetched_at,omitempty"`
	OK bool `json:"ok"`
}

// summaryPacket is the optional terminal `_summary` line per
// ADR-0023 §2: aggregate stats the daemon logs for observability.
type summaryPacket struct {
	Summary summaryFields `json:"_summary"`
}

type summaryFields struct {
	Ingested int `json:"ingested"`
	Errors int `json:"errors"`
	DurationMs int `json:"duration_ms"`
}

// errorPacket is the `_error` control-packet shape per ADR-0023 §2.
// Used when the auth/dial pre-poll setup fails — the binary
// surfaces a single `_error` line and exits non-zero. Per-message
// failures inside the poll cycle accumulate into the final
// `_summary.errors` count rather than emit per-message `_error`
// lines, matching the conventions yaad-bgg + yaad-wikipedia
// established for cycle-level vs per-envelope failure shape.
type errorPacket struct {
	Error errorFields `json:"_error"`
}

type errorFields struct {
	Slug string `json:"slug,omitempty"`
	Kind string `json:"kind"`
	Message string `json:"message"`
}

// runCommandFetch is the `--command fetch` handler per ADR-0022 §1
// + ADR-0023. Reads operator config from env vars, dials Gmail via
// IMAP, runs ONE poll cycle, and emits NDJSON envelopes per message
// (one JSON line each, terminated with `\n`, written to stdout
// before the next IMAP operation). At end of cycle, emits a
// `_summary` packet with aggregate stats.
//
// Per-message commit semantics (existing Poller behavior preserved
// from a prior PR): the `yaad-ingested` Gmail label flips immediately
// after each emit returns nil. A crash mid-cycle leaves
// already-emitted messages on-disk via the daemon (write-as-you-go
// per ADR-0023 §recovery) AND label-flipped on Gmail's side; the
// next cycle picks up where this one left off.
//
// Auth-required env vars (account_email, app_password) MUST be set;
// missing either surfaces as a single `_error` line + non-zero exit.
// Per-envelope binary attachments (MIME parts) are deferred to
// yaad-index (follow-up MIME-walking + staging issue).
func runCommandFetch(ctx context.Context, stdout, stderr io.Writer) error {
	start := time.Now()

	cfg, err := loadIMAPConfig()
	if err != nil {
		writeErrorPacket(stdout, "config_invalid", err.Error())
		return err
	}

	ingestedLabel := envOrDefault(EnvIngestedLabel, gmail.DefaultIngestedLabel)
	skipLabel := envOrDefault(EnvSkipLabel, gmail.DefaultSkipLabel)

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := gmail.Dial(ctx, cfg)
	if err != nil {
		writeErrorPacket(stdout, "imap_dial_failed", err.Error())
		return err
	}
	defer func() { _ = client.Close() }()

	enc := json.NewEncoder(stdout)
	now := time.Now().UTC().Format(time.RFC3339)

	emit := func(_ context.Context, env gmail.IngestEnvelope) error {
		// Encoder.Encode writes the JSON value followed by a
		// trailing `\n` — exactly the NDJSON shape the daemon's
		// json.Decoder consumes one value at a time. stdout is
		// unbuffered (os.Stdout default), so the line reaches
		// the daemon's pipe immediately, before MarkIngested fires.
		return enc.Encode(buildSourceLine(env, now))
	}

	poller := gmail.NewPoller(client, ingestedLabel, skipLabel, emit, logger)
	count, cycleErrs := poller.Tick(ctx)

	// Terminal `_summary` packet per ADR-0023 §2. Optional but
	// recommended — daemon logs the aggregate at the close of
	// each cycle for observability.
	_ = enc.Encode(summaryPacket{
		Summary: summaryFields{
			Ingested: count,
			Errors: len(cycleErrs),
			DurationMs: int(time.Since(start).Milliseconds()),
		},
	})

	if len(cycleErrs) > 0 {
		// Per-message errors accumulated; surface count via stderr
		// so operators tailing logs see them. Already-emitted
		// envelopes survive (write-as-you-go); cycle continues to
		// next invocation.
		_, _ = fmt.Fprintf(stderr, "yaad-gmail: cycle completed with %d error(s)\n", len(cycleErrs))
	}
	return nil
}

// buildSourceLine maps a Poller-emitted IngestEnvelope to the
// per-message NDJSON wire shape per ADR-0021 / ADR-0023. Extracted
// from runCommandFetch's emit closure so the mapping is unit-testable
// without standing up a fake IMAP client.
//
// fetchedAt is the RFC3339 timestamp the daemon stamps as the
// provenance entry's `fetched_at` field; runCommandFetch passes
// `time.Now().UTC().Format(time.RFC3339)` once per cycle so all
// emitted envelopes share the cycle-start timestamp.
func buildSourceLine(env gmail.IngestEnvelope, fetchedAt string) sourceLine {
	// Daemon derives the entity ID via slug.Slug(name); we emit the
	// already-slugged form (gmail.SourceSlug result) as `name` so
	// the daemon's slugify is idempotent and produces the same
	// SourceID. The `gmail:` namespace prefix is stripped — the
	// daemon re-prepends from SourceNamespace at ingest time.
	nameOnly := env.SourceID
	if strings.HasPrefix(nameOnly, gmail.SourceNamespace+":") {
		nameOnly = nameOnly[len(gmail.SourceNamespace)+1:]
	}

	data := map[string]any{
		"subject": env.Subject,
		"date": formatRFC3339(env.Date),
	}
	if len(env.Body) > 0 {
		data["body"] = string(env.Body)
	}

	return sourceLine{
		OK: true,
		Structured: &structuredEnvelope{
			Kind: gmail.UniversalSourceKind,
			Name: nameOnly,
			Data: data,
			Edges: edgesToBlock(env.Edges),
			Provenance: []provenanceJSONEntry{{
				Source: "gmail:fetch",
				FetchedAt: fetchedAt,
				OK: true,
			}},
		},
	}
}

// edgesToBlock converts the Poller's flat Edge list into the
// ADR-0021 polymorphic edges block keyed by edge type. Multiple
// targets per type collapse into a list.
func edgesToBlock(in []gmail.Edge) map[string][]edgeTargetJSON {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]edgeTargetJSON, len(in))
	for _, e := range in {
		out[e.Type] = append(out[e.Type], edgeTargetJSON{Name: e.Name, Kind: e.Kind})
	}
	return out
}

// loadIMAPConfig reads the operator-config env vars and produces an
// IMAPConfig. account_email + app_password are required; host +
// port default to imap.gmail.com:993 unless overridden.
func loadIMAPConfig() (gmail.IMAPConfig, error) {
	cfg := gmail.IMAPConfig{
		AccountEmail: os.Getenv(EnvAccountEmail),
		AppPassword: os.Getenv(EnvAppPassword),
		Host: os.Getenv(EnvIMAPHost),
	}
	if portStr := os.Getenv(EnvIMAPPort); portStr != "" {
		var port int
		if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
			return cfg, fmt.Errorf("gmail: invalid %s=%q: %w", EnvIMAPPort, portStr, err)
		}
		cfg.Port = port
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func envOrDefault(env, def string) string {
	if v, ok := os.LookupEnv(env); ok {
		return v
	}
	return def
}

func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// writeErrorPacket emits a single `_error` control packet per
// ADR-0023 §2. Used for setup-time failures (auth, dial) where
// the binary can't even start the IMAP poll cycle. The daemon's
// onControl handler logs this; the binary's exit code (returned by
// the caller) signals the failure to subprocess.Plugin.Stream.
func writeErrorPacket(stdout io.Writer, kind, msg string) {
	_ = json.NewEncoder(stdout).Encode(errorPacket{
		Error: errorFields{
			Kind: kind,
			Message: msg,
		},
	})
}
