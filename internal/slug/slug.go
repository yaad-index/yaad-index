// Package slug owns the daemon-side slug utility per ADR-0021
// (slug ownership moves from plugin to daemon).
//
// Responsibility split (the operator's design correction, 2026-05-09):
//
// - Plugins (and agents acting as plugin-extensions during fill)
// own canonical-NAME production. They strip year-suffix,
// parens-disambig, and any other domain-specific annotation
// BEFORE emitting names to the daemon. yaad-bgg knows BGG
// year-suffixes; yaad-wikipedia knows Wikipedia parens-
// disambig; the daemon does not need to.
// - Daemon slug utility = slug-only. Lowercase + hyphenate +
// transliterate via gosimple/slug. NO disambig logic, NO
// year/parens regex, NO domain-specific normalization. Pure
// dumb slugifier.
//
// The single exported function `Slug(name)` is a thin wrapper over
// `github.com/gosimple/slug` so the call site stays uniform across
// emission, fill, and edge-resolution paths and the underlying
// library can be swapped without touching callers.
package slug

import gosimpleslug "github.com/gosimple/slug"

// Slug derives a deterministic slug from a descriptive name.
// Pure function, no I/O. Wraps gosimple/slug.Make.
//
// Plugins are responsible for stripping their own domain-specific
// disambig (year, parens-annotation, BGG series-separators, …)
// BEFORE calling this function. The utility does NOT know about
// year-suffix or parens-disambig — it just slugifies whatever it
// receives.
func Slug(name string) string {
	return gosimpleslug.Make(name)
}
