package writelocks

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcquire_FreshLockSucceeds pins the happy-path: first Acquire
// on a fresh artifact returns a release closure + nil error.
func TestAcquire_FreshLockSucceeds(t *testing.T) {
	t.Parallel()
	m := New()

	release, err := m.Acquire("entity:foo", "req-1")
	require.NoError(t, err)
	require.NotNil(t, release)
	defer release()

	assert.True(t, m.Holds("entity:foo"))
	assert.Equal(t, 1, m.Active())
}

// TestAcquire_ConflictReturnsTypedError pins the conflict path:
// second Acquire on the same artifact returns *ConflictError naming
// the active holder. No second lock created (Active stays 1).
func TestAcquire_ConflictReturnsTypedError(t *testing.T) {
	t.Parallel()
	m := New()

	release, err := m.Acquire("entity:foo", "req-1")
	require.NoError(t, err)
	defer release()

	release2, err := m.Acquire("entity:foo", "req-2")
	require.Error(t, err)
	require.Nil(t, release2)

	ce, ok := AsConflict(err)
	require.True(t, ok, "err should be *ConflictError; got %T: %v", err, err)
	assert.Equal(t, "entity:foo", ce.Artifact)
	assert.Equal(t, "req-1", ce.Holder, "ConflictError must name the active holder, not the requester")
	assert.False(t, ce.AcquiredAt.IsZero(), "AcquiredAt must be populated")

	assert.Equal(t, 1, m.Active(), "conflict must not create a second lock entry")
}

// TestRelease_AllowsReacquire pins the release path: after the
// holder releases, a new caller can Acquire the same artifact.
func TestRelease_AllowsReacquire(t *testing.T) {
	t.Parallel()
	m := New()

	release, err := m.Acquire("entity:foo", "req-1")
	require.NoError(t, err)
	release()
	assert.False(t, m.Holds("entity:foo"))

	release2, err := m.Acquire("entity:foo", "req-2")
	require.NoError(t, err)
	require.NotNil(t, release2)
	defer release2()
	assert.True(t, m.Holds("entity:foo"))
}

// TestRelease_IsIdempotent pins that calling release twice is safe;
// the second call is a no-op. Necessary because handlers wrap
// release in defer + may also call it explicitly on success paths.
func TestRelease_IsIdempotent(t *testing.T) {
	t.Parallel()
	m := New()

	release, err := m.Acquire("entity:foo", "req-1")
	require.NoError(t, err)
	release()
	release() // second call — must not panic, must not drop a different holder's lock
	assert.False(t, m.Holds("entity:foo"))
}

// TestRelease_AfterReacquireDoesNotDropNewHolder pins the
// defensive double-release guard: if Acquire-Release-Acquire-then-
// stale-Release happens across goroutines, the stale release must
// NOT drop the new holder's lock. The lock pointer identity check
// catches this race.
func TestRelease_AfterReacquireDoesNotDropNewHolder(t *testing.T) {
	t.Parallel()
	m := New()

	release1, err := m.Acquire("entity:foo", "req-1")
	require.NoError(t, err)
	release1()
	// req-2 acquires while req-1's release closure still exists.
	release2, err := m.Acquire("entity:foo", "req-2")
	require.NoError(t, err)
	defer release2()
	// Stale release from req-1 — must not drop req-2's lock.
	release1()
	assert.True(t, m.Holds("entity:foo"),
		"stale release from a prior holder must not drop the current holder's lock")
}

// TestAcquire_DistinctArtifactsDoNotCollide pins the per-artifact
// keying: two different artifacts can both be held simultaneously.
func TestAcquire_DistinctArtifactsDoNotCollide(t *testing.T) {
	t.Parallel()
	m := New()

	releaseA, err := m.Acquire("entity:foo", "req-1")
	require.NoError(t, err)
	defer releaseA()

	releaseB, err := m.Acquire("entity:bar", "req-2")
	require.NoError(t, err)
	defer releaseB()

	assert.Equal(t, 2, m.Active())
	assert.True(t, m.Holds("entity:foo"))
	assert.True(t, m.Holds("entity:bar"))
}

// TestAcquire_ConcurrentSameArtifact pins thread-safety: under N
// concurrent goroutines acquiring the same artifact, exactly one
// succeeds and the rest get *ConflictError. The successful goroutine
// then releases; a subsequent post-release acquire succeeds.
func TestAcquire_ConcurrentSameArtifact(t *testing.T) {
	t.Parallel()
	m := New()

	const N = 100
	var successes atomic.Int32
	var conflicts atomic.Int32
	var wg sync.WaitGroup
	releaseCh := make(chan func(), 1)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release, err := m.Acquire("entity:hot", "req")
			if err != nil {
				if _, ok := AsConflict(err); ok {
					conflicts.Add(1)
				}
				return
			}
			successes.Add(1)
			releaseCh <- release
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(1), successes.Load(),
		"exactly one acquire must succeed across N concurrent contenders")
	assert.Equal(t, int32(N-1), conflicts.Load(),
		"the rest must get conflicts")

	// Drain + release; subsequent acquire succeeds.
	(<-releaseCh)()
	release, err := m.Acquire("entity:hot", "req-post")
	require.NoError(t, err)
	defer release()
}

// TestAcquire_UGCSectionKeyShape documents the UGC section-keying
// convention. Two writers on different sections of the same UGC
// file get distinct artifact keys → both succeed.
func TestAcquire_UGCSectionKeyShape(t *testing.T) {
	t.Parallel()
	m := New()

	releaseA, err := m.Acquire("user-content:books-i-loved#fiction", "req-1")
	require.NoError(t, err)
	defer releaseA()

	releaseB, err := m.Acquire("user-content:books-i-loved#non-fiction", "req-2")
	require.NoError(t, err)
	defer releaseB()

	assert.Equal(t, 2, m.Active(),
		"different sections of the same UGC file produce distinct artifact keys")
}

// TestAcquire_UGCSectionVsWholeFileDontConflict documents a real
// trade-off: section-keyed acquire and whole-entity acquire on the
// same underlying file DO NOT collide at the lock-manager layer
// (the keys are different strings). The OS-rename layer is the
// final serializer — last-writer-wins on the underlying file.
//
// This is intentional in v1 per the issue's "block-on-conflict
// per artifact" framing: the artifact is the key, not the
// underlying file. UGC operators rarely cross-mutate frontmatter
// and a section concurrently; if it surfaces, a future iteration
// can add a coarser file-level interlock.
func TestAcquire_UGCSectionVsWholeFileDontConflict(t *testing.T) {
	t.Parallel()
	m := New()

	releaseSection, err := m.Acquire("user-content:books-i-loved#fiction", "req-1")
	require.NoError(t, err)
	defer releaseSection()

	// Whole-file write (e.g. frontmatter) uses a different key shape.
	releaseFile, err := m.Acquire("user-content:books-i-loved", "req-2")
	require.NoError(t, err)
	defer releaseFile()

	assert.Equal(t, 2, m.Active(),
		"section-keyed + whole-file-keyed acquires on the same file are distinct keys (documented v1 trade-off)")
}

// TestIsConflict_OnNonConflict pins that IsConflict returns false
// on non-ConflictError errors (e.g. wrapped I/O errors). Handlers
// branch on it; a false positive would silently 409 on an
// unrelated failure.
func TestIsConflict_OnNonConflict(t *testing.T) {
	t.Parallel()
	other := errFromString("some other failure")
	assert.False(t, IsConflict(other))
	assert.False(t, IsConflict(nil))
}

// errFromString returns an error with the given string — minimal
// non-ConflictError type for the IsConflict test.
type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }

func errFromString(s string) error { return &stringErr{s: s} }
