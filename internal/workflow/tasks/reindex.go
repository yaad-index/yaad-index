// Startup reindex per #268: walks the tasks/ directory at
// daemon start and ensures every task file has a backing
// `task:<slug>` row in the store. Vault remains authoritative
// per ADR-0008 — the row is rebuilt from the file when missing,
// preserved (no Data clobber) when already present.

package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/store"
)

// IndexFromVault walks every task file under the reader's
// tasks/ directory and upserts the matching `task:<slug>`
// entity row into st when not already present. Pre-existing
// rows are skipped so prior store-side mutations (operator
// fills, set_property writes) survive the reindex.
//
// Returns the number of newly-materialized rows (purely
// informational for operator startup logs). Per-task errors
// log at WARN and continue so a single malformed file doesn't
// block the rest of the index pass.
//
// Safe to call multiple times — idempotent across re-invocations
// (re-run on second startup re-sees the rows it created on
// first startup and short-circuits each).
func IndexFromVault(ctx context.Context, st store.Store, reader *Reader, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if st == nil || reader == nil {
		return 0, nil
	}
	summaries, err := reader.List(ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("tasks: list for reindex: %w", err)
	}
	created := 0
	for _, s := range summaries {
		taskID := canonical.TaskKind + ":" + s.ID
		if _, err := st.GetEntity(ctx, taskID); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			logger.WarnContext(ctx, "tasks reindex: store probe failed",
				"task_id", taskID, "err", err)
			continue
		}
		data := map[string]any{
			"workflow":   s.Workflow,
			"created_at": s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if s.Subject != "" {
			data["subject"] = s.Subject
		}
		if s.DedupKey != "" {
			data["dedup_key"] = s.DedupKey
		}
		if s.Errored {
			data["errored"] = true
		}
		if err := st.UpsertEntity(ctx, &store.Entity{
			ID:   taskID,
			Kind: canonical.TaskKind,
			Data: data,
		}); err != nil {
			logger.WarnContext(ctx, "tasks reindex: upsert failed",
				"task_id", taskID, "err", err)
			continue
		}
		created++
	}
	return created, nil
}
