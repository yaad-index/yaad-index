# ADR-0003 — CLI library: kong

**Status:** Accepted (2026-04-28)
**Date:** 2026-04-28
**Depends on:** [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md)

## Context

ADR-0001 locks Go as the runtime. The server binary needs a CLI surface — at minimum a `serve` command, plus admin/utility commands likely to land alongside it (`kinds list`, `ingest <url>` for one-shot debugging, `version`). Future plugin work (separate ADR) will add a subcommand dispatch layer on top.

The decision is which CLI parsing library to use. Three classes of options:

1. **Stdlib `flag`** — zero deps, works for trivial cases.
2. **A struct-tag based parser** like [`alecthomas/kong`](https://github.com/alecthomas/kong).
3. **A command-builder lib** like [`spf13/cobra`](https://github.com/spf13/cobra), often paired with [`spf13/viper`](https://github.com/spf13/viper) for config layering.

This ADR picks one and explains why.

## Decision

**Use [`alecthomas/kong`](https://github.com/alecthomas/kong).**

### Why kong

- **Struct-based declaration.** Flags live as struct fields with struct tags. The type system enforces correctness — a `--log-level` of type `string` constrained to a known enum, or a `--bind` of type `string` validated as a `host:port` literal, is parsed and validated without extra code. What you read in the struct *is* the flag surface; no separate command-builder scaffolding.
- **Global + subcommand pattern is native.** A driver scenario like `yaad-index --config=X serve --bind=localhost:7433` maps to a top-level struct with global fields plus embedded subcommand structs. kong handles argument partitioning, help generation, and error reporting. This scales as we add subcommands without adding boilerplate per command.
- **One dependency.** `cobra` typically pulls `viper` for config-file layering. We don't want viper's config chain (`system → global → local → env → flag`) competing with whatever override chain we land on (env-and-flag-only is the likely v1 shape). One library for flag parsing is the cleaner fit.
- **Tag-driven env binding.** `env:"YAAD_INDEX_BIND"` on a struct field binds the flag to the env var with no wrapper code. Matches the pattern any future plugin work will want.
- **Help + usage come free.** kong auto-generates `--help` per subcommand. No manual `-h` handling.
- **Small surface area.** kong's public API is small enough that upgrades are rarely disruptive. Maintained deliberately small by its author.

### Why not the alternatives

- **`stdlib flag` + `FlagSet`-per-subcommand.** Works for a one-flag binary; painful as soon as we need globals + subcommands. Manual argument partitioning at every subcommand, no help generation, env binding is DIY, version flag is DIY. Acceptable for a script; not for the long-lived CLI surface this server will grow.

- **`spf13/cobra` (± `spf13/viper`).** The de-facto default for big Go CLIs. cobra alone handles commands and flag parsing fine; it doesn't *require* viper. But its env-var binding isn't built in — the idiomatic addition is viper, and viper's config chain duplicates what we'd otherwise pin in our own ADR. The alternative — cobra without viper, with hand-wired env binding — gives up the thing kong does natively. cobra's command-builder style (`cobra.Command{Use: ..., Run: ...}`) is also more verbose than kong's struct tags once subcommand count grows. For a personal tool, this is overhead.

- **`alecthomas/kingpin`.** Same author as kong; kong is explicitly kingpin's successor. Picking kingpin means adopting a deprecated lib.

- **`urfave/cli`.** Equally valid alternative. Kong preferred for two concrete reasons: (a) env binding is more compact — kong uses `env:"YAAD_INDEX_BIND"` in the struct tag, urfave/cli takes `EnvVars: []string{"YAAD_INDEX_BIND"}` on a flag-definition struct sitting apart from the target variable; (b) help rendering on nested commands is more structured in kong (deeper hierarchies print cleanly). If we ever migrate away from kong, urfave/cli is where we go.

- **`rsc/getopt`, `jessevdk/go-flags`, `pflag` standalone.** None handle the subcommand + global pattern cleanly; all would require scaffolding.

## Consequences

**Positive**
- Single, small CLI dependency.
- Flag surface is self-documenting via struct tags.
- Help + env binding without wrapper code.
- The subcommand pattern scales as the binary grows.

**Negative**
- One non-stdlib dep where we could have had zero (with `flag`).
- Ties us to one library; if kong stops being maintained, migration to urfave/cli is the documented escape hatch.

## Open questions

None at this time.

## Action items if approved

1. Add `github.com/alecthomas/kong` to `go.mod`.
2. Define the top-level CLI struct in `cmd/yaad-index/main.go` with global flags (`--config`, `--log`, `--log-level`, `--version`) plus `serve` as the first subcommand. Subsequent subcommands (`kinds`, `ingest`, etc.) land alongside. The default bind address is pinned in [ADR-0001](./0001-fresh-rewrite-ai-first-remote-api.md); `--bind` (env: `YAAD_INDEX_BIND`) overrides it.
3. Document the `env:"YAAD_INDEX_*"` naming scheme for env-bound flags.
