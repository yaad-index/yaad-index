# ADR-0023: Unified plugin response protocol — NDJSON streaming, one envelope per line

## Status

Accepted 2026-06-06 (proposed 2026-05-10).

## Depends on

- [ADR-0005](./0005-plugin-lifecycle.md) — plugin invocation model (subprocess-per-request, JSON over stdio, `--init` capabilities). This ADR replaces that ADR's single-entity response clause with a streaming protocol.
- [ADR-0022](./0022-plugin-command-protocol.md) — plugin command-protocol (commands field, `!` invocation sigil, in-memory job system, routing-time validation). This ADR replaces that ADR's `envelopes: [...]` array clause with the same streaming protocol applied uniformly.

## Context

Plugin responses today come in two distinct shapes at the wire level:

1. **URL-shape (per ADR-0005)**: plugin returns a single JSON object describing one source-entity, exits. `wikipedia: Tehran` produces one entity. `bgg: ticket to ride` produces one entity. The 1:1 contract is hard-coded into the daemon's source-emission reader.
2. **Command-shape (per ADR-0022)**: plugin returns `{envelopes: [env1, env2, ..., envN]}` — an array of N entity emissions, exits. The N:1 invocation:emission shape was introduced for yaad-gmail's `!fetch` (one fetch invocation produces many email entities).

The two-shape distinction at the response layer creates ongoing protocol drift:

- **Daemon-side**: the source-emission reader needs two parsing paths (single-blob vs array). Future plugins doing N-result fetches from URL-shape inputs (RSS aggregators, archive crawlers, search APIs that return multiple hits) would need a third accommodation or be forced into command-shape against intent.
- **Plugin-side**: authors decide between shapes at the response layer based on invocation shape, conflating two concerns.
- **Memory**: command-shape's batch-array forces the plugin to hold all N envelopes in memory before emitting; the daemon waits for the full array before any write hits disk. A 100-email Gmail fetch buffers 100 emails twice (plugin RAM + daemon parser).
- **Partial-failure recovery**: a batch response that crashes mid-build loses all N envelopes; write-as-you-go would commit the early ones. Command-shape's array shape forecloses this.

The 2026-05-10 design follow-up to ADR-0022 surfaced these pressures concretely. The fix is to drop the URL-vs-command distinction at the response layer: every plugin emits the same line-by-line stream, regardless of invocation shape. Multiplicity becomes the natural axis (N = 0 | 1 | many) instead of two separate contracts.

The `!` sigil and the URL-vs-command discrimination remain at the *invocation* layer (ADR-0022 §2). This ADR is purely about the response wire format.

## Decision

### 1. NDJSON streaming on stdout

All plugin responses are newline-delimited JSON (NDJSON) on stdout. Plugins emit one self-contained JSON object per line, flush after each line, and exit when done. Number of lines per invocation is N: zero (no result), one (typical), or many (yaad-gmail fetch, future iterative plugins).

```
{"source_kind": "wikipedia", "slug": "Tehran", "frontmatter": {...}, "body": "..."}
```

(typical 1-line response; yaad-wikipedia, yaad-bgg today)

```
{"source_kind": "email", "slug": "<msgid_dot_xxx>", "frontmatter": {...}, "body": "..."}
{"source_kind": "email", "slug": "<msgid_dot_yyy>", "frontmatter": {...}, "body": "..."}
{"_error": {"slug": "<msgid_dot_zzz>", "kind": "parse", "message": "malformed MIME boundary"}}
{"source_kind": "email", "slug": "<msgid_dot_qqq>", "frontmatter": {...}, "body": "..."}
{"_summary": {"ingested": 3, "errors": 1, "duration_ms": 4521}}
```

(typical N-line response; yaad-gmail fetch)

### 2. Line shapes

Three line shapes are recognized:

- **Source emission** — top-level fields `source_kind`, `slug`, `frontmatter`, `body`, plus any plugin-specific extensions per ADR-0014 / ADR-0015 / ADR-0021. Same field set plugins emit today; just per-line instead of bundled.
- **Error sentinel** — top-level field `_error`, payload `{slug?, kind, message}`. Plugin emits this when it chose to skip an envelope but continue the invocation. Daemon logs and increments error-count; does not abort the stream.
- **Summary packet** — top-level field `_summary`, payload `{ingested, errors, duration_ms}`. Optional, terminal. Plugin emits as the last line when it has aggregate stats. Daemon uses for ingest-stats / observability. If a plugin mistakenly emits a `_summary` line mid-stream, the daemon treats the first one as the stats close-signal; subsequent source-emission lines after it are still processed normally (a misplaced summary doesn't truncate the stream). Recommended (not required): emit `_summary: {"ingested": 0, "errors": 0, ...}` even when N=0, so a "found nothing" invocation is distinguishable from a crashed plugin that exited before emitting.

Lines starting with `{"_` are reserved for control packets. Source emissions never use leading-underscore top-level fields.

Lines that fail to parse as JSON are logged and skipped (defensive; keeps a malformed line from terminating the stream).

**Attachments**: each source-emission line MAY carry an `attachments[]` array per [ADR-0014](./0014-plugin-attachment-contract.md). Streaming does not change the attachment contract — the three URI schemes (`file://`, `https://`, `base64://`) and the daemon's stage-then-copy semantics are unchanged. Plugins choose per-attachment per ADR-0014 §scheme-selection; for size-heavy attachments (email payloads, archive captures, etc.) `file://` via the operator-configured staging directory is the preferred scheme.

### 3. Failure mode: continue-with-errors

Plugins SHOULD continue emitting after a per-envelope error. Reject-whole-batch is the wrong failure mode for background or iterative fetches: one bad MIME structure shouldn't block all new mail; one transient API hiccup on entity 3 of 50 shouldn't lose entities 4–50.

Plugin MAY exit non-zero if the WHOLE invocation failed (auth error, network down, no IMAP connection). Daemon treats non-zero exit as invocation failure but does NOT roll back lines emitted before exit — the write-as-you-go contract guarantees that anything that hit disk stays.

Plugin MAY emit an `_error` line for a per-envelope failure and continue; this is the typical case.

### 4. Recovery via plugin-side idempotency

The plugin's source slug is the idempotency key. Re-invocations skip already-written slugs (existing daemon behavior — slug is a primary key in the source table). For yaad-gmail the slug is the RFC-822 Message-ID; for URL-shape plugins the slug is what the existing source-write path uses.

Restart-safety on partial completion is a property of write-as-you-go + slug dedup:

- Plugin emits envelopes 1..7 successfully, crashes before emitting 8..10.
- Lines 1..7 are committed to disk (write-as-you-go).
- Plugin exits non-zero; daemon logs invocation failure.
- Next invocation re-runs the fetch. Plugin emits all 10 envelopes (it has no state about what was emitted last time).
- Daemon's slug dedup writes only 8..10 (1..7 already exist as source-entities); 1..7 are no-ops.
- End state: all 10 emails are on disk; one extra plugin run; no daemon-side checkpoint primitive needed.

For yaad-gmail specifically, the additional wrinkle is Gmail's own server-side label state (the `yaad-ingested` label is set as part of the per-message commit). Re-runs walk the un-labeled set; already-labeled messages are skipped at IMAP fetch time, before they ever reach an envelope. Plugin and daemon both have idempotency layers; either is sufficient on its own.

### 5. Daemon-side reader

The daemon's source-emission reader switches from "decode one blob from stdout" to "decode one line at a time as they arrive." Each line is probed for the `_error` and `_summary` control-packet shapes; lines that match neither are decoded as source emissions and routed through the existing entity-write path. Plugin process exit closes stdout and terminates the read loop. Implementation detail; the load-bearing contract is the wire format above.

### 6. Out of scope

- **Streaming back-pressure / rate-limiting**: a future ADR if a plugin emits faster than the daemon can write.
- **Daemon → plugin streaming inputs**: this ADR is plugin → daemon only.
- **Mid-stream cancellation**: future ADR if a plugin needs to abort a partially-emitted invocation cleanly.
- **Schema evolution of the line format**: existing source-emission field set carries forward unchanged; future field additions follow the existing additive convention.

## Migration

### Existing plugins

The live yaad-index plugin allowlist (per ADR-0006 config + `/etc/yaad-index/config.yaml`) is `yaad-wikipedia` and `yaad-bgg`. Both print one JSON object on stdout + exit today. Update each to `print + \n + flush + exit`. One-line change each. Behavior unchanged from the daemon's perspective (still 1 line per invocation, still 1 file). Response contents are byte-identical except for the trailing newline.

`yaad-gmail` (in active development for) implements the streaming protocol natively from its first version; no migration step.

### Daemon

- Replace the source-emission reader's `json.Unmarshal` of the full subprocess stdout with the line-buffered scanner described in §5.
- Update the existing routing-time validation (ADR-0022 §4) to remain unaffected — validation runs against the `--init` capabilities, not the response stream.
- Capability cache plumbing (ADR-0022 §5) is unaffected — `commands: [...]` field round-trip is independent of response shape.

### Tests

- Existing 1-emission plugin tests need a trailing-newline assertion only. The 1:1 case stays the dominant path.
- New N-emission tests for yaad-gmail land with (yaad-gmail commands declaration); cover the 0-line, 1-line, N-line, and N-with-`_error` cases.
- Crash-mid-stream test fixture lands in the daemon-side reader test suite to pin write-as-you-go.

## Supersedure

This ADR supersedes:

- **ADR-0005's single-entity response clause** — plugins are no longer constrained to one entity per invocation. ADR-0005's other sections (subprocess lifecycle, --init contract, stderr conventions, exit-code semantics) remain in force.
- **ADR-0022's `envelopes: [...]` array clause (§3)** — command-shape responses use the same NDJSON stream as URL-shape. ADR-0022's other sections (commands field, `!` sigil, in-memory job system, routing-time validation, CLI surface) remain in force.

Both ADRs are kept open with a "Superseded in part by ADR-0023" note in their Status sections (PR amends those notes).

## Consequences

**Positive**:

- One protocol instead of two; daemon-side reader collapses to a single path.
- Memory bound on N: plugin holds one envelope at a time, daemon writes one at a time. Gmail fetch with 100 emails buffers 1, not 100.
- Progressive commit on disk → partial-failure recovery is a property of the protocol, not a per-plugin add-on.
- Future plugins that need N-shape from URL-shape inputs (RSS aggregators, search-API plugins, archive crawlers) get it without further protocol work.
- Observability improves: `_summary` packets carry per-invocation aggregate stats the daemon can record.

**Negative**:

- Existing plugins need a small migration commit (one line each: ensure trailing newline + flush).
- Daemon reader becomes line-scanner instead of one-shot read; slightly more code, slightly more error surface (line-too-long handling). Trade-off accepted.
- Plugin authors must remember to flush after each line; a buffered stdout that hasn't flushed could lose envelopes on early exit. Mitigated by language-runtime defaults (`fmt.Println` flushes for line-buffered terminals; explicit `os.Stdout.Sync()` recommended at strategic points).

**Neutral**:

- Wire format is human-readable; debugging via `tail -f` works on plugin output during dev.

## Refs

- ADR-0005 — plugin lifecycle invocation model (single-entity response clause superseded)
- ADR-0006 — plugin discovery + url_patterns dispatch (untouched)
- ADR-0014 — plugin attachment contract (carries forward unchanged; per-envelope `attachments[]` with `file://` / `https://` / `base64://` schemes)
- ADR-0021 — daemon-owned slug; canonical entities are edge-target labels (slug semantics carry forward)
- ADR-0022 — plugin command-protocol (envelopes-array clause superseded; everything else in force)
- yaad-gmail — first consumer of the streaming protocol (gmail integration, merged 2026-05-10)
- yaad-index — yaad-gmail commands declaration (will land against the streaming protocol, not the array shape)
