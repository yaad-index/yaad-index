package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/yaad-index/yaad-index/internal/buildinfo"
)

// healthResponse is the wire shape for GET /v1/health. Intentionally
// thin — the endpoint exists so monitors can verify the HTTP layer is
// alive, NOT to surface any application-layer health information.
// Adding a `database: "ok"` field would tempt monitors into treating
// /v1/health as a deep-readiness probe; the store has its own
// integrity guarantees and a separate readiness check (when one
// lands) is the right venue for those.
type healthResponse struct {
	OK bool `json:"ok"`
	Version string `json:"version,omitempty"`
}

// handleHealth returns a fixed-shape 200 response identifying this
// build. Version is best-effort — runtime/debug.ReadBuildInfo returns
// (nil, false) for binaries built without module info (e.g. some
// `go test` paths); in that case the field is omitted via omitempty.
//
// The handler does not touch the store or registry; the implicit
// liveness signal is "the HTTP layer accepted a connection and ran a
// handler." Operators wanting deep readiness checks should add a
// /v1/readiness endpoint in a later ADR.
func handleHealth(logger *slog.Logger) http.HandlerFunc {
	version := readBuildVersion()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(healthResponse{
			OK: true,
			Version: version,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/health response", "err", err)
		}
	}
}

// readBuildVersion picks the operator-visible build identifier with
// the precedence (per the source issue):
//
// 1. `buildinfo.Version` if the Makefile injected one via `-ldflags -X`.
// This is the primary path: `make build` stamps `<git-describe>+
// <short-hash>` so a binary running on a deployment can name itself.
// 2. `runtime/debug.ReadBuildInfo` module version, for binaries built
// via `go install` / `go run` / IDEs that don't pass through the
// Makefile's ldflags but still embed module metadata.
// 3. Empty string (omitted from the wire response via `omitempty`)
// when neither is available — `go test` paths typically end here.
//
// Computed once at handler-construction time — version is immutable per
// process, no point re-reading on every request.
//
// Defense in depth: the prefix check on `buildinfo.Version` rejects
// not just the bare "unknown" sentinel but ANY value starting with it
// (e.g. the "unknown+unknown" emitted when both git
// fallbacks fired). The Makefile is fixed to degrade to the bare
// sentinel in those cases, but the prefix guard makes layer-2
// resilience explicit so a future Makefile regression can't silently
// land "unknown..." on the wire.
func readBuildVersion() string {
	if v := buildinfo.Version; v != "" && !strings.HasPrefix(v, buildinfo.Unknown) {
		return v
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}
