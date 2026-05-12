// Package buildinfo carries build-time-injected identity data for the
// yaad-index binary. Today that's just `Version` — the git-derived
// `<tag>+<short-hash>` string the Makefile passes via `-ldflags -X`
// at link time.
//
// Set at link time by `make build` (see Makefile). Tests assert the
// fallback path works when the var isn't injected — go-test invocations
// don't pass through the Makefile's ldflags, so unit tests see the
// zero value and fall through to runtime/debug.ReadBuildInfo (or the
// "unknown" sentinel).
//
// Out of scope per the source issue: build timestamp, hostname, semver
// enforcement. If a future ADR adds those, they live here too.
package buildinfo

// Version is the operator-visible build identifier emitted on
// `GET /v1/health`. The Makefile sets this to `<git-describe>+<short-hash>`
// at link time (see the LDFLAGS variable in the Makefile). When the
// binary is built without that injection — `go install`, `go test`,
// IDEs invoking `go build` directly — the var stays at its sentinel
// `"unknown"` and callers fall through to runtime/debug.ReadBuildInfo
// via api.readBuildVersion.
//
// `var` (not `const`) so the linker can rewrite it via `-X`. The
// `"unknown"` sentinel doubles as the wire value when no other
// version source is available either, so /v1/health always emits
// something operator-readable.
var Version = Unknown

// Unknown is the sentinel Version value when no build-time injection
// happened. Exposed as a const so the precedence helper can check
// against it without copying the literal in two places.
const Unknown = "unknown"
