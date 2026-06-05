# CEL surface reference for workflow authors

The single, complete reference for the CEL expression vocabulary available in
workflow patterns — every variable binding, every custom function, where each
can be used, and the gotchas. If you hit a CEL name a workflow uses and want to
know what it means, it's here.

> **Source of truth.** This file is hand-maintained against the workflow engine
> — `internal/workflow/decision/` (`decision.go`, `temporal.go`, `graph_walk.go`,
> `regex_capture.go`), `internal/workflow/engine/` (trigger context), and
> `internal/workflow/template/` (templating). When you add or change a CEL
> binding, function, or templating rule in those packages, **update this file in
> the same PR.** The Go declarations are authoritative; this doc is the
> author-facing index over them.

CEL is the [cel-go](https://pkg.go.dev/github.com/google/cel-go) expression
language; the workflow engine uses it for decision logic per ADR-0024. For the
workflow YAML shape and the action catalog, see [workflows.md](workflows.md);
this doc is only the expression surface.

## 1. Where CEL expressions appear

Every workflow string field is either a CEL expression or a mustache template
(see §2). The fields:

| Field | Expected result | Default if omitted |
|---|---|---|
| `condition` | `bool` (bare-CEL) | always fires |
| `subject` | `string` | `entity.id` |
| `context[].via` | any (`dyn`, bare-CEL) | — (bound to the context name) |
| `dedup.key` | `string` | workflow + entity pair |
| `task_append.content`, `add_note.{target,content}`, `add_gap.entity`, `add_canonical_edge.{source,target_name,data[*]}`, `set_property.{entity,fields[*]}`, `archive_entity.{entity,reason}`, `restore_entity.{entity,reason}`, `task_resolve.{subject,match_key}` | `string` | id fields default to `entity.id` |
| `plugin_dispatch.args[*]` | varies | **special rule — see §6** |

A `condition` or `context[].via` is always a bare-CEL expression. The other
string fields are CEL-by-default but switch to mustache when they contain `{{`
(§2). A non-`string` result in a `string` field is stringified with Go's default
representation; a non-`bool` `condition` raises an eval error (which becomes an
error-task).

## 2. Template modes: bare-CEL vs mustache

`internal/workflow/template/` decides the mode per field:

- **No `{{` in the source → bare-CEL.** The whole string compiles as one CEL
  expression returning a string. `subject: entity.id` evaluates the expression.
- **Contains `{{` → mustache.** Literal text interleaved with `{{ <cel> }}`
  segments, each compiled separately: `content: 'PR {{ entity.data.number }} merged'`.

**The dash hazard (bare-CEL only).** In bare-CEL mode `word-word` parses as
subtraction between two identifiers. `subject: github-active` reads as
`github - active` → "undeclared reference to 'github'". Fixes:

- quote it as a CEL string literal: `subject: '"github-active"'`
- or switch to mustache: `subject: '{{ "github-active" }}'`

The same applies to `+`, `*`, `/` between word characters. When a field's value
is a literal string with punctuation, prefer mustache.

## 3. Variable bindings

### `entity` — the triggering entity

Always present (an empty map for a manual trigger with no entity). Fields:

- `entity.id` — canonical id, e.g. `github-pr:owner_repo_123_456`
- `entity.kind` — entity kind, e.g. `github-pr`, `boardgame`, `day`
- `entity.slug` — slugified name
- `entity.data` — map of plugin-enriched fields; plugin data lives **under
  `.data`**, e.g. `entity.data.title`, `entity.data.state`

```cel
condition: 'entity.kind == "github-pr" && entity.data.state == "open"'
```

### `edge` — the triggering edge (`edge_created` only)

Populated only on `edge_created` triggers; an empty map otherwise (guard with
`has(edge.type)`). Fields:

- `edge.type` — canonical edge type (`is_about`, …)
- `edge.from` / `edge.to` — source / target entity ids
- `edge.from_title` / `edge.to_title` — denormalized titles (empty if resolution failed)
- `edge.timestamp` — CEL timestamp; wrap with `string(edge.timestamp)` to embed

```cel
condition: 'has(edge.type) && edge.type == "is_about"'
```

### `trigger` — what caused this firing

Always present. Fields:

- `trigger.source` — the fully-resolved entity whose action initiated the event
  (empty map on a resolution miss; equals `entity` for self-triggered events)
- `trigger.event` — the bus event: `entity_created`, `entity_updated`,
  `edge_added`, `fill_completed`, `manual`
- `trigger.timestamp` — CEL timestamp of the originating event
- `trigger.cause` — sub-event detail: the changed field for `entity_updated`
  (e.g. `data.state`), the edge type for `edge_added`, the gap name for
  `fill_completed`; empty for `entity_created` / `manual`
- `trigger.old_value` / `trigger.new_value` — the field's previous / new value,
  **`entity_updated` only**; `has()` reports false on every other trigger type
  (#456)

```cel
condition: 'trigger.event == "entity_updated" && trigger.old_value == "open" && trigger.new_value == "closed"'
```

### Context bindings — your own named variables

Each `context[]` entry binds its `via` expression's result to a name, visible to
`condition`, `subject`, `dedup.key`, and every action template. Names can't
shadow `entity`, `edge`, or `trigger`. Bindings evaluate in order, so a later
`via` may reference an earlier binding.

```yaml
context:
  - name: prior
    via: 'has(entity.data.previous_id) ? graph.try_get(entity.data.previous_id) : {}'
condition: 'has(prior.rating) && prior.rating > 7'
```

## 4. Per-trigger-type availability

Which bindings are populated for each trigger type:

| Trigger | `entity` | `edge` | `trigger.old/new_value` | `trigger.cause` |
|---|---|---|---|---|
| `entity_created` | the new entity | empty | omitted | empty |
| `entity_updated` | the changed entity | empty | **populated** | changed field name |
| `edge_created` | the edge's from-entity | **populated** | omitted | edge type |
| `fill_completed` | the filled entity | empty | omitted | gap name |
| `manual` | the input entity (if resolved) or empty | empty | omitted | empty |

`trigger.source` and `trigger.timestamp` are populated for all types;
`trigger.event` always names the type.

## 5. Functions

### Graph lookup

- **`graph.get(id) → entity map | null`** — fetch a canonical entity
  (`{id, kind, slug, data?}`). A miss returns `null` **and records a MissingRef**
  (attached to any task the workflow produces — see §7). Guard with `!= null`.
- **`graph.try_get(id) → entity map | {}`** — has-safe sibling. A miss returns an
  **empty map** and records **no** MissingRef, so `has(graph.try_get(id).field)`
  is false rather than a null-traversal error (#456).

```cel
graph.get("github-pr:owner_repo_123_456").data.state == "closed"
has(graph.try_get("author-id").is_verified) ? graph.try_get("author-id").is_verified : false
```

### Pattern matching

- **`regex_capture(text, pattern, group_index) → string`** — RE2 capture-group
  extraction; group 0 is the whole match. Returns `""` on no-match / out-of-range.
  Literal patterns are validated at workflow-registration time; compiled regexes
  are process-cached.

```cel
regex_capture(entity.id, "github:[^_]+_([^_]+)_(?:pr|issue)_([0-9]+)", 1)  # → "repo"
```

### Day anchors (current moment, daemon timezone)

All return a day-id string `day:YYYY-MM-DD`. Evaluated once per fire from a single
clock snapshot, so every callsite in one firing agrees (midnight-safe); operator
`timezone:` changes take effect on the next fire.

- **`today()`**, **`yesterday()`**, **`tomorrow()`**

### Current period

- **`this_week() → "YYYY-Www"`** (ISO 8601, Monday-start)
- **`this_month() → "YYYY-MM"`**
- **`this_year() → "YYYY"`**

### Date arithmetic

- **`add_days(day_id, n) → day_id`** — signed offset; leap-aware, cross-month/year correct
- **`days_between(day_a, day_b) → int`** — signed day count, positive when `day_b` is later

```cel
add_days(today(), 7)
days_between(entity.data.created_on, today()) > 30
```

### Period ⇄ day expansion

- **`days_in_week(week_id) → list`** (7 day-ids, Mon–Sun)
- **`days_in_month(month_id) → list`** (28–31, leap-aware)
- **`days_in_year(year_id) → list`** (365 / 366)
- **`week_of(day_id) → "YYYY-Www"`** — ISO-week-**year** (so `day:2025-12-29` → `2026-W01`)
- **`month_of(day_id) → "YYYY-MM"`**
- **`year_of(day_id) → "YYYY"`**

### Graph walking

Each returns a wrapper `{items: list, truncated: bool, total: int}` — the
truncation flag rides with the data. `truncated` is true when `total >
len(items)`; the per-call cap defaults to 1000 (operator-overridable via
`workflow.graph_walk_cap`). There is no CEL pagination primitive — walks that
must be exhaustive should check `truncated` and page via the REST API.

- **`graph.in_edges(id[, edge_type])`** / **`graph.out_edges(id[, edge_type])`** —
  edges terminating at / originating from `id`; `items` are
  `{from, to, type, metadata}`
- **`graph.in_neighbors(id[, edge_type])`** / **`graph.out_neighbors(id[, edge_type])`** —
  the entities on the other end (one batch fetch, not N+1); `items` are
  `{id, kind, slug, data?}`

```cel
graph.out_neighbors(entity.id, "designed_by").items.map(p, p.slug)
```

### String / list extensions + casts

- **`ext.Strings`** methods on strings: `.split(sep)`, `.replace(old,new)`,
  `.substring(start[,end])`, `.lowerAscii()`, `.upperAscii()`, `.indexOf(sub)`,
  and `.join(sep)` on a list.
- **`ext.Lists`**: `.flatten()` collapses `list<list<T>>` → `list<T>`.
- **CEL natives** (no extension): `.map`, `.filter`, `.exists`, `.all`, `.size()`.
- **`string(v)`** — type cast; `string(<timestamp>)` formats RFC3339
  (`"2026-05-17T19:00:00Z"`), the way to embed `edge.timestamp` / `trigger.timestamp`.

```cel
days_in_week(this_week()).map(d, graph.out_neighbors(d, "logged").items).flatten()
entity.id.split(":")[1]            # the kind from a canonical id
string(edge.timestamp).substring(0, 10)   # date only
```

## 6. `plugin_dispatch.args` — mustache-only / literal-default (#456)

Unlike every other templated field, `plugin_dispatch.args` values do **not**
default to CEL. The rule:

- a string carrying a `{{ … }}` segment is rendered as a template;
- a **bare** string (no `{{`), a number, a bool, or a nested value passes through
  to the plugin **verbatim**.

Plugin args are literal-heavy (paths, modes, flags), so this keeps existing
literal args quote-free and back-compatible. Rendered args are keyed `arg:<name>`
internally (mirroring `set_property`'s `field:<name>`).

```yaml
plugin_dispatch:
  plugin: yaad-fetch
  command: refetch
  args:
    id: "{{ entity.id }}"   # templated — has {{ }}
    mode: refetch-only      # literal — passes through verbatim
    limit: 3                # number — not templatable, verbatim
```

## 7. Missing-reference handling

- `graph.get(id)` miss → `null` **and** a deduplicated MissingRef recorded for
  `id` (one per id per evaluation, sorted for determinism). The workflow does not
  halt; the note is attached to any task it produces, so the operator can add the
  edge or ingest the entity.
- `graph.try_get(id)` miss → empty map, **no** MissingRef. Use it when a missing
  reference is an expected, silent case rather than something to surface.

## 8. Result-type coercion

| Field | Expected | On mismatch |
|---|---|---|
| `condition` | `bool` | eval error → error-task |
| `subject`, `dedup.key`, action template fields | `string` | non-string stringified (e.g. `42` → `"42"`) |
| `context[].via` | any | bound as-is, no coercion |
