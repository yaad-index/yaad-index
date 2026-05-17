package api

import (
	"net/http"

	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// acquireWriteLock is the handler-side helper for the per-artifact
// write-lock acquired before any vault mutation surface (per yaad-
// index #23 + ADR-0024 §Concurrent writes).
//
// On success returns (release, true). Caller MUST defer release to
// free the lock for the next writer. On conflict emits a 409
// envelope naming the active holder + the artifact, returns (nil,
// false), and the handler stops.
//
// The holder string identifies the writer in the conflict message
// the NEXT caller sees. Built from the request claim (operator +
// agent identity) so the operator can correlate the rejection with
// an in-flight request.
//
// **When to call:** every handler that produces a vault.Writer
// invocation. Skip for additive surfaces (notes, edges) per the
// #23 spec — those don't conflict at the storage layer.
func acquireWriteLock(w http.ResponseWriter, r *http.Request, mgr *writelocks.Manager, artifact string) (release func(), ok bool) {
	holder := writeHolderForRequest(r)
	release, err := mgr.Acquire(artifact, holder)
	if err != nil {
		if ce, isConflict := writelocks.AsConflict(err); isConflict {
			writeError(w, http.StatusConflict, "write_conflict",
				"artifact \""+ce.Artifact+"\" is currently being written by \""+ce.Holder+
					"\" (acquired "+ce.AcquiredAt.Format("2006-01-02T15:04:05Z07:00")+"); retry")
			return nil, false
		}
		// Non-conflict Acquire errors aren't a thing today (the
		// Manager only returns *ConflictError or nil), but defensive:
		// surface unknown errors as 500 so a future expansion of the
		// primitive doesn't silently 200.
		writeError(w, http.StatusInternalServerError, "internal_error",
			"failed to acquire write lock: "+err.Error())
		return nil, false
	}
	return release, true
}

// writeHolderForRequest derives the human-readable holder identifier
// from the request's auth claim + request ID. Surfaces in the 409
// envelope the next caller sees so they can correlate the rejection
// with an actor + a specific in-flight request.
//
// Format: "<request-id> / agent:<sub> operator:<op>" — request ID
// first so log greps land on the lock-holder quickly. When the
// claim is unauthenticated / anonymous, the agent/operator portion
// collapses to "anonymous".
func writeHolderForRequest(r *http.Request) string {
	rid := RequestIDFromContext(r.Context())
	if rid == "" {
		rid = "no-request-id"
	}
	claim, ok := ClaimFromContext(r.Context())
	if !ok || claim == nil {
		return rid + " / anonymous"
	}
	if IsAnonymousClaim(claim) {
		return rid + " / anonymous (auth-disabled)"
	}
	return rid + " / agent:" + claim.Subject + " operator:" + claim.Operator
}
