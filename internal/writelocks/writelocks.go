// Package writelocks is the per-artifact daemon write-lock manager
// per yaad-index #23 + ADR-0024 §Concurrent writes.
//
// **Block-on-conflict, fail-fast.** Acquire returns immediately:
// either with a release function on success, or with a
// *ConflictError on conflict naming the current holder. There is
// no queuing, no merging, no last-writer-wins, and no waiter pool.
// Callers surface conflicts as 409 envelopes; the operator (or
// agent) retries.
//
// **Per-artifact, not global.** The lock is keyed on an artifact
// string the caller computes. Convention:
//
//   - Whole-entity writes use the entity ID (e.g. "wikipedia:tehran").
//   - UGC section writes scope to the (id, section) pair (e.g.
//     "user-content:books-i-loved#fiction"). Two writers touching
//     different sections of the same UGC file collide on the file
//     at the OS-rename layer, but the section-scoped key surfaces
//     a clearer error and matches the operator's mental model
//     ("this section is being edited").
//
// **In-process only.** This is a single-daemon primitive; multi-
// daemon distributed locking is out of scope per #23's "Out of
// scope" list. A future ADR may add a vault-side advisory-lock
// extension if a multi-host deployment surfaces.
//
// **No automatic expiry.** Acquired locks are held until the caller
// invokes the returned release function. A wedged caller that
// never releases leaves the artifact permanently locked until
// daemon restart — acceptable v1 trade-off because all callers in
// the daemon today wrap Acquire in a defer-release pair. If a
// wedge appears in practice, a future iteration adds a heartbeat
// + sweep, tracked under #23's follow-up.
//
// **Skip-cases.** Two write classes deliberately skip the lock per
// the #23 spec: additive comments (POST /v1/entities/{id}/comments)
// and additive edges (POST /v1/edges). Both are append-only at
// the storage layer; concurrent appenders don't conflict. Lock
// acquisition is the caller's responsibility, so those handlers
// simply don't call Acquire.
package writelocks

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/clock"
)

// Manager is the per-artifact write-lock manager. Construct via
// New(); the zero value is not usable.
type Manager struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	holder     string
	acquiredAt time.Time
}

// New constructs an empty Manager. The returned pointer is safe for
// concurrent use; each Acquire / Release pair takes a short
// internal mutex to consult the locks map.
func New() *Manager {
	return &Manager{locks: make(map[string]*lockEntry)}
}

// ConflictError is returned by Acquire when the requested artifact
// is currently held by another writer. The handler converts this
// into a 409 envelope naming both the artifact and the active
// holder, so the operator can correlate the rejection with the
// in-flight request.
type ConflictError struct {
	Artifact   string
	Holder     string
	AcquiredAt time.Time
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("write conflict: artifact %q is locked by %q (acquired %s)",
		e.Artifact, e.Holder, e.AcquiredAt.Format(time.RFC3339))
}

// IsConflict reports whether err (or any wrapped error in its
// chain) is a *ConflictError. Handlers use it to discriminate the
// 409 path from other write-failures (vault I/O, DB upsert, etc.).
func IsConflict(err error) bool {
	var ce *ConflictError
	return errors.As(err, &ce)
}

// AsConflict extracts the *ConflictError from err if present. Same
// shape as errors.As; callers use it to pull the Holder/Artifact
// fields out of the chain. Returns nil + false on non-conflict
// errors.
func AsConflict(err error) (*ConflictError, bool) {
	var ce *ConflictError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// Acquire attempts to lock artifact for holder. On success returns
// a release closure the caller MUST invoke (typically via defer)
// to free the lock. Multiple release calls are safe (idempotent —
// subsequent calls are no-ops).
//
// On conflict returns (nil, *ConflictError) without modifying the
// lock map. The caller surfaces the error via the 409 path; the
// next attempt is the operator's / agent's responsibility.
//
// holder is a short identifier that surfaces in ConflictError —
// typically a request-id + the actor identity (e.g. "req-abc /
// agent:sora"). Keep it human-readable; it's what the next caller
// sees on their 409.
func (m *Manager) Acquire(artifact, holder string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.locks[artifact]; ok {
		return nil, &ConflictError{
			Artifact:   artifact,
			Holder:     existing.holder,
			AcquiredAt: existing.acquiredAt,
		}
	}
	entry := &lockEntry{
		holder:     holder,
		acquiredAt: clock.Now().UTC(),
	}
	m.locks[artifact] = entry
	var released bool
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if released {
			return
		}
		// Defensive: only release our own entry. A pathological
		// double-release shouldn't drop a different holder's lock
		// in the race window where Acquire-Release-Acquire by
		// another goroutine intervened.
		if cur, ok := m.locks[artifact]; ok && cur == entry {
			delete(m.locks, artifact)
		}
		released = true
	}, nil
}

// Holds reports whether artifact is currently locked. Diagnostic
// only — handlers don't gate on this (the Acquire result is the
// authoritative answer, free of TOCTOU windows). Used by tests +
// observability surfaces.
func (m *Manager) Holds(artifact string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.locks[artifact]
	return ok
}

// Active returns the count of currently-held locks. Diagnostic for
// metrics / debugging surfaces.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}
