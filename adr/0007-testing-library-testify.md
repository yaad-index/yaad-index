# ADR-0007 — Testing assertion library: testify

**Status:** Accepted (2026-05-01)
**Date:** 2026-05-01
**Depends on:** [ADR-0004](./0004-logging-library-slog.md) (the precedent for "stdlib unless a third-party gives a real win")

## Context

Every test in this repo is currently written with the stdlib `testing` package and hand-rolled `if got != want { t.Errorf(...) }` patterns. As of 2026-05-01: 25 test files, ~211 `Test*` functions, ~309 if-pattern assertion lines. Three friction points have been steady costs:

- **Verbose diagnostics.** A single field mismatch costs five lines (`if got.X != want.X { t.Errorf("X: want %q, got %q", want.X, got.X) }`). Multi-field response-shape checks balloon to 30+ lines per test, and the noise drowns the actual assertion intent.
- **No standard split between fatal-on-failure and continue-after-failure.** Tests mix `t.Fatalf` (stop) and `t.Errorf` (collect) ad hoc. Setup invariants (DB open, JSON decode) are sometimes `Errorf`, leading to nil-pointer panics on the next line; response-shape checks are sometimes `Fatalf`, hiding additional failures that would have helped triage.
- **Slice / map equality is hand-coded everywhere.** Each file has its own `equalStrings` / `provenanceEqualByValue` / `indexProvenance` shaped helpers. They drift; a few have subtle bugs around order-sensitivity vs. order-insensitivity.

A shared assertion library standardises the diagnostic output, gives the fatal-vs-continue distinction a name, and removes the ad-hoc helpers in favour of one well-tested one. That said: this is purely a developer-experience change. No production code path imports the chosen library; the trust boundary doesn't move.

## Decision

**Adopt `github.com/stretchr/testify`** as the standard assertion library for new and existing tests.

Two sub-packages are in scope:

- **`testify/require`** — assertions that abort the test on failure. Used for setup invariants where a failure makes the rest of the test meaningless: opening the in-memory store, decoding a JSON response into the expected envelope, fetching a row that the test just seeded.
- **`testify/assert`** — assertions that record the failure and let the test continue. Used for response-shape checks where multiple field mismatches each carry diagnostic value: every field of an `entity` response, every option in a disambiguation list, every provenance entry's flags.

`testify/mock` and `testify/suite` are out of scope. The repo already has table-driven tests + per-file fixtures; a mocking framework would solve a problem we don't have, and `suite` adds setup/teardown ceremony that `t.Cleanup` already handles cleanly.

### Conventions

These ride alongside the decision so the refactor has one consistent shape across the 25 test files:

- **`require.*` for setup invariants.** A failure means the test cannot proceed without nil-deref / panic / spurious downstream assertion. Examples: `require.NoError(t, err, "store.New")` after opening the store, `require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))` after a JSON decode, `require.Len(t, got.Edges, 2)` before indexing into the slice.
- **`assert.*` for response-shape checks.** A failure should be diagnostically additive, not test-stopping. Examples: each field of a parsed response struct, each entry in an options map, each derived value from a search hit.
- **`assert.Equal(t, want, got)` parameter ordering.** testify's convention is `expected, actual` — counter to most stdlib hand-rolled patterns where `got` came first. Match testify's order even though the param names look misleading; failure messages assume `expected` first and read confusingly otherwise.
- **Skip the `assert.New(t)` / `require.New(t)` shortcut.** It saves a `t,` per call but creates per-file inconsistency when one file uses the shortcut and the next doesn't. Use the package-level functions uniformly; if a single file accumulates >50 calls and the visual weight becomes a problem, judge per file.
- **Keep stdlib for non-assertion APIs.** `t.Helper()`, `t.Parallel()`, `t.Run()`, `t.Setenv()`, `t.Cleanup()`, `t.TempDir()`, `t.Log*()` — all unchanged. testify only replaces the comparison-and-fail pattern, not the test scaffolding.
- **Drop the ad-hoc compare helpers.** `equalStrings`, `provenanceEqualByValue`, `indexProvenance`-style maps, `entityNames` slice extractors — all become `assert.Equal` / `assert.ElementsMatch` / `assert.Contains` calls. Files that still need a structural transform (e.g. extracting a string slice from a struct slice for a length check) keep the helper but use testify for the comparison.
- **Skip `msgAndArgs` on `assert.Equal` when the comparison expression already names the field.** `assert.Equal(t, want.ID, got.ID, "id")` carries no information beyond `assert.Equal(t, want.ID, got.ID)` — testify's failure message already prints the receiver. Carry `msgAndArgs` only for disambiguation in loops (`assert.Equal(t, want[i], got[i], "i=%d", i)`) or for adjacent identical-shape asserts where the message distinguishes which one fired (e.g. `"page1" / "page2" / "page3"` over a paginated test). The same lean applies to assertions whose receiver expression is itself self-documenting (`assert.NotEmpty(t, got.FillToken)`); reserve free-form messages for genuine context the reader cannot recover from the expression.
- **`require.Error` vs. `assert.Error` follows the same precondition lens as the no-error case.** Use `require.Error` when the rest of the test reads from `err` (e.g. `assert.Contains(t, err.Error(), "duplicate")`, or `errors.As`-style unwrapping) — a missing error makes those follow-ups nil-deref noise. Use `assert.Error` for independent multi-case rejection batteries (three sequential `assert.Error` lines covering empty/nil/zero arguments, with no follow-up reading any individual `err`) and for terminal asserts at end-of-test. The mirror rule applies to `assert.NoError` vs. `require.NoError`: `require.NoError` when the next line uses the result that the call returned, `assert.NoError` when the no-error check is itself the assertion. Don't go fishing for cases that aren't there — apply at sites where the precondition pattern is already visible.

### Alternatives considered

- **`github.com/google/go-cmp`** — best-in-class struct diffing, used inside one of testify's own backends. Rejected as the primary library because it doesn't have the `require` / `assert` split: every `cmp.Diff` returns a string the caller logs themselves. Adding `require`/`assert` semantics on top of `go-cmp` is a third-party-and-a-half — testify's split is the central value here. (Tests that need a deep struct diff can still call `cmp.Diff` inside an `assert.Equal` / `assert.True` — testify and go-cmp compose.)
- **`github.com/matryer/is`** — minimal "one assertion" library, no fatal/continue split. Same rejection reason as go-cmp.
- **Continue with stdlib + a small in-house helper package.** Considered in the spirit of ADR-0004 ("stdlib unless …"). Rejected because the friction is across 211 functions and growing; a hand-rolled package would re-derive what testify already ships and tested. ADR-0004's scope is the production binary, where every dep is a deployed-byte concern. Test-only deps don't ship.

### Consequences

#### Positive

- **Diagnostics are standardised.** Failure messages name the field, show expected vs. actual, and include the per-call `msgAndArgs` context. Triage-by-CI-log gets faster.
- **The fatal-vs-continue split is named.** `require` vs. `assert` is the contract; reviewers can flag misuse at the right node.
- **Ad-hoc helpers go.** ~10 files stop carrying their own `equalStrings`-shape helpers; the shapes that survive are real structural transforms (e.g. project a slice of structs to a slice of strings for a length check), not equality checks.

#### Negative / costs

- **One more dep in `go.mod`.** testify is widely used (tens of millions of installs), MIT-licensed, single-maintainer-org but with broad community contribution. Risk of abandonment is real but low; the failure mode is a fork + global rename, weeks of work but not load-bearing on the production binary.
- **Production binary size.** None — testify is imported only from `*_test.go` files, so the linker excludes it from `cmd/yaad-index`. The Consequences note here exists so an operator inspecting `go.mod` sees a third-party dep and can verify "no, that doesn't ship."
- **Refactor churn.** Two PRs ( PR-A locks conventions on `internal/store`; PR-B mechanically applies them to the remaining ~166 functions). PR-B is a deliberate exception to the one-package-per-PR rule because there's no logic change in any package — every PR-B file diff is `if a != b { t.Errorf(...) }` → `assert.Equal(t, b, a)` style. The issue spec authorises the exception.

## Action items if approved

1. Add `github.com/stretchr/testify` to `go.mod` (regular `require`, no `// test` tag; tooling treats it as a normal dep).
2. PR-A — refactor `internal/store/*_test.go` (8 files / 45 functions) to lock the conventions in code. Pattern-establishing: this package surfaces the four assertion shapes the rest of the codebase will hit (error paths, struct value compares, slice-element compares, map-element checks).
3. PR-B — apply the same conventions across `internal/api`, `internal/plugins`, `internal/config`, `cmd/yaad-index`. Mechanical; reviewers focus on convention adherence, not behaviour change.
4. AGENTS.md gains a "Testing" section pointing at this ADR + summarising the require/assert split for future contributors.
