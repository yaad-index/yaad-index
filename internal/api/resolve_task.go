// Resolution-task resolve flow per #304 Cut C3.3 — the
// final slice. Routes an operator's option pick from the
// MCP / HTTP `task_resolve` surface through the plugin
// shorthand re-ingest, then either CreateEdge (no prior
// edge exists for the source-edge tuple) or
// UpdateEdgeTarget (a prior edge points at a stale target;
// Cut B's 409 path surfaces when the tuple shifted under
// us). Archives the on-disk resolution-task on success via
// the existing tasks.Writer.Resolve pathway.
//
// Per the locked Cut C3 design (Gap 6 → option (b)): no
// edge is materialized at workflow-fire time; the edge
// only lands when the operator picks an option here. The
// `update_edge_target` path is reserved for the legitimate
// stale-rewrite shape (something — another workflow,
// another resolve — landed an edge against this source
// tuple in the meantime; we redirect it to the chosen
// target).

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yaad-index/yaad-index/internal/store"
)

// resolutionTaskFM mirrors the on-disk frontmatter shape
// FileTaskWriter.WriteResolutionTask renders per Cut C3.1.
// Parsed on-demand here rather than re-using the tasks
// package's reader because that reader's struct doesn't
// surface the resolution-specific fields (the C3.1 design
// kept the typed shape inside the workflow/actions package
// to avoid widening the existing reader's surface).
type resolutionTaskFM struct {
	Kind                string                  `yaml:"kind"`
	SchemaVersion       int                     `yaml:"schema_version"`
	IdempotencyKey      string                  `yaml:"idempotency_key"`
	FromID              string                  `yaml:"from_id"`
	EdgeType            string                  `yaml:"edge_type"`
	TargetKind          string                  `yaml:"target_kind"`
	ResolverPlugin      string                  `yaml:"resolver_plugin"`
	NormalizedRawTarget string                  `yaml:"normalized_raw_target"`
	RawTarget           string                  `yaml:"raw_target"`
	Options             []resolutionTaskFMOption `yaml:"options"`
}

type resolutionTaskFMOption struct {
	ID      string `yaml:"id"`
	Label   string `yaml:"label,omitempty"`
	Summary string `yaml:"summary,omitempty"`
}

// parseResolutionTaskFile reads + yaml-decodes the resolution-
// task frontmatter at <tasksDir>/<id>.md. Returns an error
// when the file is missing, has no frontmatter fence, or
// carries a `kind:` other than `resolution-task`. Path-
// traversal-safe: ids with `/` or `\` reject early.
func parseResolutionTaskFile(tasksDir, id string) (*resolutionTaskFM, error) {
	if strings.ContainsAny(id, "/\\") {
		return nil, fmt.Errorf("resolution-task id %q contains path separator", id)
	}
	path := filepath.Join(tasksDir, id+".md")
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("resolution-task %q: not found", id)
		}
		return nil, fmt.Errorf("read resolution-task %q: %w", path, err)
	}
	const opener = "---\n"
	const closer = "\n---\n"
	if !strings.HasPrefix(string(body), opener) {
		return nil, fmt.Errorf("resolution-task %q: missing frontmatter fence", id)
	}
	rest := string(body)[len(opener):]
	idx := strings.Index(rest, closer)
	if idx < 0 {
		return nil, fmt.Errorf("resolution-task %q: unterminated frontmatter", id)
	}
	var fm resolutionTaskFM
	dec := yaml.NewDecoder(strings.NewReader(rest[:idx]))
	dec.KnownFields(false)
	if err := dec.Decode(&fm); err != nil {
		return nil, fmt.Errorf("resolution-task %q: yaml decode: %w", id, err)
	}
	if fm.Kind != "resolution-task" {
		return nil, fmt.Errorf("resolution-task %q: kind is %q, not resolution-task", id, fm.Kind)
	}
	return &fm, nil
}

// resolveOptionInTask returns the option whose id matches
// optionID, or an error when no option matches. Operator
// picks that aren't in the recorded option list are
// rejected — the workflow's recorded "these are the
// candidates" is the authoritative set.
func resolveOptionInTask(fm *resolutionTaskFM, optionID string) (*resolutionTaskFMOption, error) {
	optionID = strings.TrimSpace(optionID)
	if optionID == "" {
		return nil, errors.New("option is required for resolution-task resolve")
	}
	for i := range fm.Options {
		if fm.Options[i].ID == optionID {
			return &fm.Options[i], nil
		}
	}
	return nil, fmt.Errorf("option %q is not in the recorded options for this task", optionID)
}

// resolveTaskEdgeOutcome names what the resolve handler did
// at the edge layer so the wire envelope can surface it
// to the caller. `created` is the fresh-edge path, `rewritten`
// is the stale-target Cut B update_edge_target path, and
// `unchanged` is the idempotent "edge already points at
// chosen" no-op.
type resolveTaskEdgeOutcome string

const (
	resolveEdgeCreated   resolveTaskEdgeOutcome = "created"
	resolveEdgeRewritten resolveTaskEdgeOutcome = "rewritten"
	resolveEdgeUnchanged resolveTaskEdgeOutcome = "unchanged"
)

// resolveResolutionTaskEdge performs the resolve-time edge
// work after the chosen entity is materialized. Branches on
// "does a prior edge exist for the (from, type) tuple":
//
//   - No prior edge → store.CreateEdge with chosen target.
//   - One prior edge with To == chosen → no-op (idempotent).
//   - One prior edge with To != chosen → store.UpdateEdgeTarget
//     to redirect (Cut B's 409 ErrEdgeStale surfaces here if
//     the tuple shifted between the probe and the update).
//
// Returns the chosen target id + the outcome enum so the
// wire envelope can name what happened.
func resolveResolutionTaskEdge(ctx context.Context, st store.Store, fromID, edgeType, chosenID string) (string, resolveTaskEdgeOutcome, error) {
	edges, err := st.GetEdgesFor(ctx, fromID, []string{edgeType})
	if err != nil {
		return "", "", fmt.Errorf("probe existing edges from %s of type %q: %w", fromID, edgeType, err)
	}
	var prior *store.Edge
	for i := range edges {
		if edges[i].Type == edgeType {
			e := edges[i]
			prior = &e
			break
		}
	}
	if prior == nil {
		if err := st.CreateEdge(ctx, &store.Edge{Type: edgeType, From: fromID, To: chosenID}); err != nil {
			return "", "", fmt.Errorf("create resolved edge %s -[%s]-> %s: %w", fromID, edgeType, chosenID, err)
		}
		return chosenID, resolveEdgeCreated, nil
	}
	if prior.To == chosenID {
		return chosenID, resolveEdgeUnchanged, nil
	}
	if _, err := st.UpdateEdgeTarget(ctx, fromID, edgeType, prior.To, chosenID); err != nil {
		return "", "", fmt.Errorf("rewrite stale edge %s -[%s]-> (was %s) → %s: %w",
			fromID, edgeType, prior.To, chosenID, err)
	}
	return chosenID, resolveEdgeRewritten, nil
}

// resolveResolutionTaskRequest is the wire shape POST
// `/v1/tasks/{id}/resolve` accepts for the resolution-task
// branch. The legacy (text-task) resolve path uses an empty
// body — when `option` is non-empty we route through here.
type resolveResolutionTaskRequest struct {
	Option string `json:"option"`
}

// resolveResolutionTaskResponse is the wire envelope the
// resolution-task branch returns. Mirrors the legacy
// taskResolveResponse keys for {ok, id, auto_archived,
// resolved_at} so callers can dispatch uniformly; adds
// resolution-specific fields naming the chosen entity +
// edge outcome.
type resolveResolutionTaskResponse struct {
	OK           bool   `json:"ok"`
	ID           string `json:"id"`
	AutoArchived bool   `json:"auto_archived"`
	ResolvedAt   string `json:"resolved_at"`
	ChosenID     string `json:"chosen_id"`
	EdgeOutcome  string `json:"edge_outcome"`
	FromID       string `json:"from_id"`
	EdgeType     string `json:"edge_type"`
	TargetKind   string `json:"target_kind"`
}

// decodeResolveOption parses the optional `option` field
// from the resolve request body. Empty body / no `option`
// field → empty string (legacy path). Reads at most 4 KB
// to keep a malformed-large-body payload from monopolizing
// the handler.
func decodeResolveOption(r *http.Request) (string, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return "", nil
	}
	const maxBody = 4 * 1024
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	var req resolveResolutionTaskRequest
	if err := dec.Decode(&req); err != nil {
		// io.EOF on empty body is fine — legacy path.
		if errors.Is(err, http.ErrBodyReadAfterClose) {
			return "", nil
		}
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) && syntax.Offset == 0 {
			return "", nil
		}
		return "", fmt.Errorf("decode resolve body: %w", err)
	}
	return strings.TrimSpace(req.Option), nil
}
