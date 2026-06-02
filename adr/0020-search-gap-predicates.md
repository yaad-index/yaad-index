# ADR-0020: Search with gap-field predicates

## Status

Proposed 2026-05-09.

## Context

A prior operator-side review identified the load-bearing build
step after ADR-0019: a search surface that can filter on
operator-filled gap values, not just on text/kind.

A motivating example query:

> "find anything this user rated higher than 5 and is a boardgame"

Today's `GET /v1/search` (per ADR-0002) supports two filter axes:

- `q` — free-text FTS query.
- `kind` — entity-kind exact match.

Neither addresses the motivating example. `data.rating > 5` is a
predicate over a gap field, not over text or kind. The current
endpoint can return all boardgames matching a text query, but it
cannot answer "boardgames I rated > 5" because there is no
language for `data.rating > 5`.

ADR-0019 made operator-filled gap values first-class data; this
ADR makes them first-class queryable. Without this, the operator
has the schema to record what they own/want/rate but no surface to
ask the database back.

### Reframe via the workflow concept

A subsequent operator-side design pass on a workflow concept
(forthcoming as a separate ADR) reframed this ADR's role:

- **Backend for workflow decision rules.** Workflow patterns
 need to evaluate predicates over operator-filled gaps when
 deciding whether to create a task (e.g., "does any subject
 have rating ≥ 7?"). The workflow engine is a deterministic
 consumer of filled data; it needs a query language to express
 those predicates. This ADR provides it.
- **User-facing task search.** Operator queries — including
 over the task entity-kind that workflows produce — use the
 same predicate language. Search across all entity kinds is a
 load-bearing capability ("if I can't search, the system is
 basically useless").

The earlier framing (search as the user's primary discovery
surface) is partially superseded: the operator's actual usage
shape is **bounded situated queries triggered by events**, not
broad ad-hoc query. The workflow framework handles the
situated-query case; this ADR covers the predicate language
workflows + operators both call into.

## Decision

Extend the search surface with **gap-field predicates** —
operator-filled values become first-class filter axes alongside
`q` and `kind`. The new shape is additive: existing callers keep
working; new callers can compose predicates as a structured
filter list.

### Query shape

Two surface changes, both on the existing `GET /v1/search`:

1. **Repeated `where` query param**, comma-or-`AND`-joined
 predicates over `data.<field>` paths. Each predicate is one
 `<path><op><value>` triple.
2. **Repeated `gap_state` query param**, filtering by the
 ADR-0019 gap state machine (`unfilled` / `agent-filled` /
 `operator-filled` / `deferred`). Default: include all *except*
 `deferred`.

Example calls:

```
GET /v1/search?kind=boardgame&where=data.rating>5
GET /v1/search?kind=boardgame&where=data.rating>=7&where=data.owned=true
GET /v1/search?kind=boardgame&where=data.played=false&where=data.want=true
GET /v1/search?kind=boardgame&where=data.rating>5&gap_state=operator-filled
GET /v1/search?where=edge.subjects=boardgame:brass-birmingham
```

The first four use `data.<path>` predicates for primitive-typed
gaps. The fifth uses an `edge.<edge-type>` predicate to find any
entity (typically a source-shape email or similar) carrying a
`subjects` edge to a specific canonical label — the form workflow
decision rules use to query canonical_type gap values (per
ADR-0021, canonical_type fills produce edges, not data-map
entries).

### Predicate grammar

Two predicate forms — `data.<path>` for primitive-typed gaps,
`edge.<edge-type>` for canonical_type gaps (which store values as
edges per ADR-0021):

```
predicate := data-pred | edge-pred
data-pred := "data." identifier ( "." identifier )* op literal
edge-pred := "edge." identifier "=" canonical-label
op := "=" | "!=" | ">" | ">=" | "<" | "<=" | "~"
literal := number | quoted-string | bool-keyword
bool-keyword := "true" | "false"
canonical-label := identifier ":" identifier
```

**Data predicates** (`data.<...>`) walk dotted-keys into the
entity's `data` map for primitive-typed gaps:

- `=` / `!=` work on string, number, bool. String comparison is
 case-sensitive exact match.
- `>` / `>=` / `<` / `<=` work on number. Applying to non-number
 field types is `400 invalid_predicate`.
- `~` is substring (case-insensitive). String-only.
- Only `data.<...>` is allowed in v1; predicates on top-level
 fields (`id`, `kind`, `summary`) use other params
 (`kind=...`, `q=...`) or land in a follow-up ADR.
- Per ADR-0021, **canonical_type gap values are NOT in the
 `data` map** — they're stored as edges. Using `data.<gap-name>`
 on a canonical_type gap returns `400 invalid_predicate` at
 parse time (the daemon knows from gap declarations which
 fields are canonical_type and detects the mismatch upfront).
 Use `edge.<gap-name>` instead.

**Edge predicates** (`edge.<edge-type>=<canonical-label>`) test
single-edge equality from the entity to a canonical label. The
edge type is the gap name (or any edge type the entity carries),
the value is a fully-qualified canonical label
(`<kind>:<slug>`):

- `where=edge.subjects=boardgame:brass-birmingham` — entities
 with a `subjects` edge to `boardgame:brass-birmingham`.
- `where=edge.designed_by=person:martin-wallace` — entities
 with a `designed_by` edge to `person:martin-wallace`.

Single-edge equality only in v1 — multi-hop edge traversal
(e.g., "find boardgames designed by people I rated > 7" which
walks edges through people back to games) is out of scope per
the bottom of this ADR.

Multiple `where` predicates AND together. OR is **not in v1** —
disjunctions are rarely needed in practice, adding them complicates
the parser and the SQL builder, and deferring keeps this ADR small. A future ADR can add `OR` if usage demands
it.

### Execution

Predicates run against the SQLite DB, not the vault. The DB is
the derived index per ADR-0008 — gap-field values live in indexed
columns (or in a JSON-indexed `data` blob with FTS5 for text and
expression indexes for the well-known fields). The handler
translates the parsed predicate list into a parameterised SQL
`WHERE` clause, executes, and returns the results.

**No vault read on the query path.** The vault is the source of
truth for state changes; the DB is what answers queries fast. If
the DB is stale (mid-reindex), the query reflects DB state; that's
a known tradeoff per ADR-0008's vault-as-truth + DB-as-derived
model.

### Interaction with ADR-0019 gap state

The ADR-0019 state machine has four states: `unfilled` /
`agent-filled` / `operator-filled` / `deferred`. The gap_state
filter:

- **Default behaviour** (no `gap_state` param): include all
 states *except* `deferred`. Deferred is operator-explicit
 "ignore for now"; surfacing them in search re-violates that.
- **Explicit `gap_state=deferred`**: surface only deferred
 entities (the operator's "what did I defer" view).
- **Explicit `gap_state=unfilled`**: useful with predicates to
 find "boardgames I haven't rated yet" (`gap_state=unfilled` +
 `kind=boardgame` over the `rating` gap — see semantic note
 below).
- **Multi-value** (`gap_state=agent-filled&gap_state=operator-filled`):
 union of states.

**Semantic note on per-field state vs entity state.** ADR-0019's
state machine is per-gap-field, not per-entity. A boardgame can
have `rating` operator-filled, `owned` unfilled, `want` deferred
all on the same entity. The `gap_state` filter applies to **the
gap-field referenced by the active where-predicates**. If
multiple `where` predicates reference different gap fields, the
filter applies to each field individually (entity matches if
every referenced gap is in an allowed state).

If `gap_state` is supplied without any `where` predicate, it
filters entities that have *any* gap in the named state.

**`gap_state` + canonical_type gap.** The ADR-0019 fill state
machine still applies to canonical_type gaps (per ADR-0021),
even though their VALUES live as edges rather than
as `data.<field>` entries. The `gap_state` filter therefore works
correctly when combined with `edge.<gap-name>` predicates:

```
GET /v1/search?where=edge.subjects=boardgame:brass-birmingham&gap_state=agent-filled
```

When `gap_state` is combined with `data.<gap-name>` where the gap
is canonical_type (a configuration mismatch — the predicate would
silently miss because the value isn't in `data`), the daemon
detects the mismatch at parse time and returns
`400 invalid_predicate` with a hint to use `edge.<gap-name>`
instead.

### API surface

No new endpoint. `GET /v1/search` gains:

- `where=<predicate>` (repeated, optional)
- `gap_state=<state>` (repeated, optional)

Existing params (`q`, `kind`, `limit`, `offset`) keep their
shape. The "at least one of `q` or `kind`" rule from ADR-0002
relaxes: at least one of `q`, `kind`, `where` is now sufficient.
A pure-where query like `where=data.rating>5` is valid (returns
across all kinds). `400 invalid_argument` only fires when *all
three* are absent.

Response shape unchanged from ADR-0002 — `{ok, results: [{id,
kind, snippet, score}], total, limit, offset}`.

### Errors

- `400 invalid_predicate` — predicate fails to parse, op
 mismatches type, path uses a top-level non-`data.` prefix,
 literal type doesn't match the op.
- `400 invalid_gap_state` — value not in
 `{unfilled, agent-filled, operator-filled, deferred}`.
- `400 invalid_argument` — all of `q`, `kind`, `where` empty.

### Pagination

`limit` / `offset` apply post-filter, same shape as today.
`total` reflects the predicate-filtered count, not the unfiltered
kind/q count.

## Consequences

### Positive

- The load-bearing query ("rated > 5 boardgames")
 becomes a single API call.
- Workflow patterns can express their decision rules in this
 predicate language without inventing a parallel query DSL.
 Both primitive-typed gap predicates (data.<...>) and
 canonical_type gap predicates (edge.<...>) are reachable from
 the same surface.
- Operator can query their task list (kind=task) with the same
 language as everything else (one query surface across all
 entity kinds, including workflow-produced tasks).
- Predicate language is small enough to spec + test + ship in
 one PR; not building an ORM.

### Negative / costs

- Additional surface area on `/v1/search`. Mitigated by
 additive-only design — existing callers untouched.
- `data.<...>` path-walking on JSON columns has a cost compared
 to flat indexed columns. For v1 the gap-field set per kind is
 small (5 for boardgame); a generated-column-per-known-gap
 approach is a follow-up if profile data shows it's needed.
- No `OR` in v1 — a future query like "rated > 7 OR owned=true"
 has to fan out to two API calls + client-side union.
- The per-gap-state vs per-entity-state semantic is subtle; the
 test plan needs to exercise both single-field and
 multi-field-different-state cases to catch confusion.

## Open questions

- **Field discovery from outside.** An agent that wants to query
 "what gap fields exist for kind=boardgame" today reads the
 plugin capabilities + canonical-vocabulary config. A dedicated
 endpoint would shorten the loop; deferring to a follow-up ADR.
- **Sort by predicate field.** `sort=data.rating&order=desc` is
 natural but adds another grammar piece. v1 stays
 relevance-sorted (FTS-driven) or insertion-ordered (no `q`).
 Sort lands in a follow-up if the operator surfaces a real need.
- **Numeric range syntax.** `data.rating BETWEEN 5 AND 8` is more
 readable than two predicates; v1 stays with two predicates,
 follow-up ADR can add range syntax if needed.

## Action items if approved

1. Implement predicate parser (Go, no external lib for v1) +
 tests for grammar edge cases.
2. Implement SQL translation for the parsed predicate list,
 parameterised to avoid injection.
3. Wire into `GET /v1/search` handler; preserve existing
 behaviour for callers without `where` / `gap_state`.
4. Update ADR-0002 with a strikethrough + cross-link to this
 ADR's amended search surface.
5. Update yaad-mcp's `search_local` tool description to surface
 the new params (no schema break — additive).
6. Document the predicate grammar in a new SKILL.md section /
 yaad-index README.

## Out of scope

- `OR` predicates. (Future ADR if usage requires.)
- Sort-by-field. (Future ADR.)
- Aggregations (`COUNT`, `AVG`). (Out of v1 entirely.)
- Search UX surface (operator-natural query interface for
 ad-hoc broad queries). The situated-query case is primary
 (workflows do bounded queries
 on events), so a dedicated user-facing search UX may not be
 needed in v1 — the agent calls /v1/search directly when the
 user asks. Defer the UX-surface ADR until usage shows whether
 it's needed.
- Salience / weighting. Separate
 architectural ADR.
- Predicates over canonical-edge traversal (e.g., "boardgames
 designed by people I rated > 7" requires walking edges
 through people to filter games). Out of v1; can extend the
 predicate grammar with edge-walks later if usage demands.
