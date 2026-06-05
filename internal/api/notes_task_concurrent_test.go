package api

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/actions"
)

// TestNotes_TaskKind_ConcurrentAppendsAllLand pins #441: with the
// per-entity write lock on the task-note append path, N concurrent
// appends all land (each retrying on the lock's fail-fast 409 so it
// re-reads the latest body). Without the lock the unguarded
// read-modify-write races and slower writes clobber faster ones, leaving
// fewer than N notes.
func TestNotes_TaskKind_ConcurrentAppendsAllLand(t *testing.T) {
	t.Parallel()
	h, st, root := newAPIWithVault(t)
	const id = "task:concurrent"
	path := seedTaskEntity(t, st, root, id, "")

	const n = 12
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			// The write lock is fail-fast (409 write_conflict on
			// contention), so a client retries until it wins the lock —
			// re-reading the latest body, which is what avoids the lost
			// update. Bounded retries guard against a stuck test.
			for attempt := 0; attempt < 500; attempt++ {
				rec := postComments(t, h, id, map[string]any{
					"text":   fmt.Sprintf("note-%02d", i),
					"author": "agent:bob",
				})
				if rec.Code == http.StatusCreated {
					return
				}
				if rec.Code == http.StatusConflict {
					time.Sleep(time.Millisecond)
					continue
				}
				assert.Failf(t, "unexpected status", "note-%02d got %d: %s", i, rec.Code, rec.Body.String())
				return
			}
			assert.Failf(t, "retries exhausted", "note-%02d never acquired the write lock", i)
		}()
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	sections, perr := actions.ParseTaskSections(string(raw))
	require.NoError(t, perr, "task body still parses as the 5-section schema after concurrent appends")
	for i := 0; i < n; i++ {
		assert.Contains(t, sections.Notes, fmt.Sprintf("note-%02d", i),
			"every concurrent append must land (no clobbering under the write lock)")
	}
}
