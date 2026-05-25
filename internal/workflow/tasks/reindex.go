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
// On each task file the helper also walks the frontmatter
// `via:` breadcrumb list and emits a `triggered_by` edge from
// the task to each non-`unknown` entity it names. This rebuilds
// the spawn-time edge surface for tasks that pre-date #268 (or
// landed via a daemon without store wiring) so
// `graph.in_neighbors(source_id, "triggered_by")` reaches them
// without requiring a task re-spawn. CreateEdge is upsert-keyed
// so re-emitting an edge that already exists is a no-op.
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
		taskCreated, err := ensureTaskRow(ctx, st, taskID, s, logger)
		if err != nil {
			continue
		}
		if taskCreated {
			created++
		}
		// Independent of row-was-just-created: walk via on every
		// pass so a row that pre-exists from a prior reindex but
		// lacks edges still gets them filled in. CreateEdge
		// upsert-dedups so re-emission is a no-op.
		emitTriggeredByFromVia(ctx, st, taskID, s.Path, logger)
	}
	return created, nil
}

// ensureTaskRow encapsulates the GetEntity-then-UpsertEntity
// dance for a single task. Returns (created, err) — created
// distinguishes the newly-materialized case from the existing
// row case so the caller can count migrations. err is non-nil
// only on a store probe failure that the caller should log +
// skip; UpsertEntity failures also log + skip but return
// (false, nil) so the outer loop continues.
func ensureTaskRow(ctx context.Context, st store.Store, taskID string, s TaskSummary, logger *slog.Logger) (bool, error) {
	if _, err := st.GetEntity(ctx, taskID); err == nil {
		return false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		logger.WarnContext(ctx, "tasks reindex: store probe failed",
			"task_id", taskID, "err", err)
		return false, err
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
		return false, nil
	}
	return true, nil
}

// emitTriggeredByFromVia walks the via breadcrumb list on the
// task file at path and emits a `triggered_by` edge per unique
// non-`unknown` Entity entry. ErrMissingEntity on the source
// side is the common case (the breadcrumb names an entity the
// store no longer has — operator pruned it, or the breadcrumb
// pre-dates the entity store entirely); skipped silently rather
// than WARNed so a noisy migration log stays readable. Other
// errors log at WARN.
func emitTriggeredByFromVia(ctx context.Context, st store.Store, taskID, path string, logger *slog.Logger) {
	sources, err := readVia(path)
	if err != nil {
		logger.WarnContext(ctx, "tasks reindex: read via failed",
			"task_id", taskID, "path", path, "err", err)
		return
	}
	for _, src := range sources {
		edge := &store.Edge{
			Type: canonical.EdgeTypeTriggeredBy,
			From: taskID,
			To:   src,
		}
		if err := st.CreateEdge(ctx, edge); err != nil {
			if errors.Is(err, store.ErrMissingEntity) {
				continue
			}
			logger.WarnContext(ctx, "tasks reindex: triggered_by edge emit failed",
				"task_id", taskID, "source_id", src, "err", err)
		}
	}
}
