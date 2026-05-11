# ADR-0001 — Fresh rewrite, AI-first, remote API

**Status:** Accepted (2026-04-27)
**Date:** 2026-04-27

## Context

The first yaad-index attempt (now ~14 ADRs, partial Go implementation, several merged PRs) was designed **human-first**: a CLI that humans run, with file-based storage, with AI agents as a possible later integration. That framing turned out to be the wrong starting point for the project's actual use.

Two facts changed the framing:

1. **The primary callers are AI agents, not humans.** AI agents (current and planned) are the ones that would use yaad-index every day to look up entities, follow edges, and store derived facts. Humans interact with the underlying markdown vault directly through their editor; they only touch yaad-index for queries the editor can't give them. The CLI was being designed for a user who wasn't going to be the heavy user.

2. **Multi-agent access is the default, not a future feature.** Multiple agents share the system from day one. File-based storage means each agent reads the same disk; that doesn't scale to remote agents on different machines, and it conflates *I have a vault on disk* with *I can answer queries about it*.

If we keep going with the existing codebase, every architectural choice we add (auth, remote access, multi-agent coordination) is a retrofit against a file-first model. The cheap path is to rebuild on the right primitive *now*, while the existing code is small enough that abandoning it costs less than refactoring it.

## Decision

**Rename the existing repo and start the rewrite fresh in a new repo under the same org.** The new repo adopts three structural commitments from day one:

### 1. AI-first

The primary interface is the API. Humans interact with the vault via their editor (already optimal) and only touch yaad-index when they want a query the editor can't give them — and they go through the same API the agents do. There is no "human CLI" track separate from the "AI tool" track. There's one surface, designed for agent calling patterns: structured requests, structured responses, idempotent operations, machine-friendly errors. A `yaad` CLI exists, but it's a thin client over the API, not a privileged path.

### 2. Remote-first API

All access — local agent, remote agent, future human-typed CLI — goes through HTTP. There is no "local file mode" that bypasses the API. Storage details (SQLite, markdown vault, indexer state) live behind the server. A client doesn't need to know the storage layout.

This is the load-bearing reversal from v0: v0 said *the markdown vault is the source of truth and yaad-index is a cache over it*. v1 says *yaad-index serves a view of the vault, and clients talk to yaad-index, not to the vault directly* (except for humans editing in their editor, which is an out-of-band write that yaad-index re-indexes).

The vault stays human-readable plain markdown — that part of v0 is right. What changes is that *agents* don't read markdown files, they call `GET /v1/entities/<id>`.

**Default bind:** the server listens on `localhost:7433` unless the operator overrides via CLI flag or env var (`--bind` / `YAAD_INDEX_BIND`). `7433` is pinned once here so downstream ADRs and docs can reference it without re-deciding. The `localhost` default keeps the network-topology trust premise honest: out of the box, only same-host clients can reach the server.

### 3. No authz in v1, by design

Every agent gets full access. Authentication and authorization are explicitly **not** in v1's scope.

**Why not skip authz?** Because adding authz later to a remote-first API is mechanical: a middleware layer, a token check, a per-route policy. Adding authz to a file-based system is structural — you'd have to introduce the API as the new chokepoint, migrate every agent off file access, and rebuild trust assumptions. The remote-first commitment is what makes "no authz now" cheap to undo.

Until authz lands, the API is on a trusted network and protected by network topology + agent-membership, not by per-call policy. Same trust model the agent bus already operates under.

## Consequences

### Positive

- **Single chokepoint for everything that follows.** Authz, rate-limiting, audit logs, schema evolution, multi-vault — all of these become "extend the API" rather than "rewrite the access model."
- **Multi-agent fan-out is free.** Agents on different hosts all hit the same endpoint. No SSH-mount, no NFS, no shared-disk assumptions.
- **Testing is mechanical.** HTTP request → assertion. No "in-process vs. out-of-process" mode-switch in the test matrix.
- **The schema becomes the contract.** Clients depend on the API surface, not on the vault's directory layout. Internal storage can change without a client migration.

### Negative / costs

- **Throwing away v0 work.** ~14 ADRs and a partial implementation become reference material, not foundation. Real cost: a few weekends of work. Mitigated by: the v0 ADRs around the entity model (0004), edge types (0005), schema validation (0006), provenance (0009) are still correct — they're concept-layer decisions and translate forward to v1.
- **A running service to operate.** v0's "one binary, no daemons" was simpler to install. v1 needs a long-running server. Mitigated by: the server is small, runs under systemd-user, and one of the things the rewrite enables is running it on a dedicated host alongside the agents, which removes the operating burden from any single user's local machine.
- **Latency floor for local clients.** A CLI that used to read SQLite directly now does a round-trip through HTTP. Mitigated by: the round-trip is ~1ms on `localhost`; the workloads are not latency-sensitive (knowledge lookup, not tight loops).
- **Bootstrap problem for the first agents.** Agents need to call the API, but until v1 ships, they fall back to direct file reads. We accept the transitional period where some access is file-based and some is API-based.

## What gets carried over from v0

These v0 ADRs survive the rewrite and remain canonical (with renumbering on import):

- 0001 — **Use Go.** Settled, not re-litigated. Go's HTTP server, concurrency primitives, and static-binary distribution fit v1's shape (long-running server + concurrent collectors + single-binary deploy on a dedicated host). The project's reviewer/contributor base is Go-fluent, which keeps review and contribution cheap.
- 0004 — Unified entity model
- 0005 — Roles as edges
- 0006 — Per-kind schema validation
- 0007 — Identity via stable canonical IDs
- 0009 — Provenance per node
- 0010 — Structured vs. unstructured collectors
- 0011 — Schema evolution

The ones that don't survive (and why):

- 0002 — *Markdown vault as source of truth*: re-frame. The vault is still authoritative for human-edited content, but yaad-index now owns the agent-facing view. ADR replacement coming.
- 0003 — *AI/tool separation*: subsumed by the API contract. No separate AI-vs-tool layer needed when there's one API.
- 0008 — *Cache-first*: API endpoints are cache-aware; the design no longer needs a dedicated ADR for "use the cache."
- 0012 — *AI client HTTP interface*: this becomes *the* interface, not a sub-interface. Folds into ADR-0002 of v1 (API surface).
- 0013 — *Personal-first, community-aware*: still true, restate after the API surface is settled.
- 0014 — *MVP plugin bundle*: re-scope. The "plugin bundle" idea was for binaries-on-PATH; v1's collectors are server-internal.

## Open questions

These need answers before the next ADR:

- **Repo naming for the renamed v0.** Suggestion: `yaad-index-v0` or `yaad-index-archive`. Operator's call.
- **Server boot path.** systemd-user on the deploy host? Container? `yaad serve` invoked manually?
- **Storage location.** SQLite per-vault, where on disk?
- **Shape of the first three endpoints to ship.** Proposed: `GET /v1/entities/<id>`, `POST /v1/ingest`, `GET /v1/search`. Other endpoints follow.

## Action items if approved

1. Rename the existing v0 repo to its archive name.
2. Initialize the new v1 repo under the org.
3. Port the surviving ADRs (0004, 0005, 0006, 0007, 0009, 0010, 0011) with renumbering.
4. Write ADR-0002 — API surface (what endpoints, what shapes, what semantics).
5. Stand up a "hello-world" server on the deploy host that returns one canned entity. End of week-one milestone.
