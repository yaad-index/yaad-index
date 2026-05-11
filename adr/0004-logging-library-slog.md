# ADR-0004 — Logging library: stdlib `log/slog`

**Status:** Accepted (2026-04-28)
**Date:** 2026-04-28
**Depends on:** [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md)

## Context

The server needs structured logging from day one. JSON output for parsing, levels for filtering, key-value pairs for context. The decision is which library to use:

1. **Stdlib `log/slog`** — added in Go 1.21, structured logging with handler pluggability.
2. **`uber-go/zap`** — third-party, faster, widely used.
3. **`rs/zerolog`** — third-party, also fast.

This ADR picks one.

## Decision

**Use stdlib `log/slog`.**

### Why slog

- **Stdlib. Zero dependency.** Every reduction in third-party deps matters. Fewer supply-chain surfaces, fewer things to update, no version pinning against a separate module. For a personal tool that ships as a binary, this matters.
- **Stable since Go 1.21.** slog has been in the standard library across multiple releases. The API is frozen; behavior is predictable across Go versions. No risk of "we've deprecated this in favor of slog v2" treadmills.
- **Structured by default.** Key-value pairs with typed attributes; JSON + text handlers built in. `slog.Info("ingested", "url", url, "kind", kind)` is the level of ergonomics we need.
- **Handler pluggability.** If a hot path ever needs zap's performance characteristics, a zap-backed `slog.Handler` exists ([Uber's `zapslog`](https://pkg.go.dev/go.uber.org/zap/exp/zapslog)). The slog API stays at call sites; the backend (stdlib handler or zap-backed) is the swappable part. The reverse direction (zap API with slog backend) is harder and not what this claims.
- **Env-driven config is straightforward.** `YAAD_INDEX_LOG_FORMAT` (`json` | `text`) and `YAAD_INDEX_LOG_LEVEL` (`debug`/`info`/`warn`/`error`) map cleanly to `slog.HandlerOptions.Level` values. A small `internal/logging.Configure(format, level string)` helper keeps initialization consistent across the binary.

### Why not zap

`uber-go/zap` is an excellent library with a real performance edge. Specifically:

- **Faster.** Typed-field builders avoid allocation hot paths; recent benchmarks show roughly 1.5–3× throughput on structured log lines compared to slog's default handler. The gap was wider at slog's 1.21 debut and has narrowed since.
- **More polished caller info.** `zap.Caller()` and stack-trace helpers are nicer.
- **Mature ecosystem.** zapcore, zapgrpc, zaptest — many integrations.

None of those advantages are load-bearing at the scale this project operates at:

- **Performance:** the bottleneck for an indexing server isn't log throughput. SQLite I/O, HTTP calls, AI extraction — these dwarf log-line serialization cost by orders of magnitude. zap's wins show up at 100k+ log lines per second, not at the hundreds-per-session this server actually does.
- **Caller info:** slog 1.21+ has caller info via `slog.HandlerOptions.AddSource`. Less polished than zap's; adequate for our debugging.
- **Ecosystem:** we don't need gRPC integration or sophisticated log shipping in v1.

The price for those non-advantages: a third-party dep, version pinning, another `go.mod` entry that could break on a Go minor-version upgrade. Net-negative for this project at this stage.

### Why not zerolog

Same story as zap with worse ergonomics for our case. `zerolog` is allocation-free for the common path and very fast, but its API is less idiomatic than slog (chained method calls vs key-value variadics). The gain over slog is performance we don't need; the cost is a non-stdlib dep.

### When we'd reconsider

Three specific signals would flip this decision:

1. We build a high-rate ingestion pipeline where log volume actually matters. Action: swap the slog handler (not the API) for a zap-backed one. Call sites unchanged.
2. We need stack-trace ergonomics for production debugging that slog can't deliver. Action: re-evaluate zap's caller helpers vs writing our own thin wrapper.
3. Go's stdlib slog stagnates and a community fork becomes the de-facto. Action: this ADR.

Until any of those: stdlib wins.

## Consequences

**Positive**
- Zero logging dependencies.
- Future-proof: slog is part of Go; won't be deprecated by an external maintainer.
- Handler swap remains a one-line change if performance ever matters.

**Negative**
- Slightly less polished caller-info reporting than zap.
- Slower than zap for high-volume hot paths. Not relevant at v1 scale; worth noting that the gap exists.

## Open questions

None at this time.

## Action items if approved

1. Confirm `log/slog` is the only logging dependency in the binary; no `zap`, `zerolog`, `logrus` imports.
2. Implement `internal/logging.Configure(format, level string) (*slog.Logger, error)` that reads from the CLI flag values (which themselves can come from env per ADR-0003) and returns a configured logger.
3. Wire the configured logger into `slog.SetDefault` at startup so all packages get structured logging via the package-level `slog.Info`/`slog.Error` calls.
