// Package tasks implements the read-side of the workflow
// task surface per ADR-0024 §"Agent surface" — list +
// load operations against the on-disk markdown task files
// the action runners (Phase 4 task_append + Phase 5.B
// err-task) write. v1 ships the filesystem-walk shape;
// entity-promotion (store rows + edges) is deferred until a
// concrete query or edge need surfaces. Single-digit-
// hundreds of tasks walk-perf budget holds for a long time
// at v1 vault sizes.
//
// Tasks live at `<vault>/tasks/<workflow-slug>-<subject-slug>.md`
// with YAML frontmatter (kind / workflow / [subject] /
// [dedup_key] / [errored] / created_at) + markdown body
// sections. The reader parses the frontmatter + extracts
// sections; the body is returned verbatim when callers need
// it.

package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TaskSummary is the lightweight per-task shape returned by
// List for the task.list surface. Avoids loading the full
// body when the caller just wants the index.
type TaskSummary struct {
	// ID is the task's canonical id — the markdown file's
	// basename without `.md`. Format
	// `<workflow-slug>-<subject-slug>` (per Phase 4
	// task_append's path scheme) or `<workflow-slug>-err`
	// (per Phase 5.B err-task).
	ID string `json:"id"`
	// Workflow is the originating workflow's name (from
	// the frontmatter `workflow:` field).
	Workflow string `json:"workflow"`
	// Subject is the rendered subject string (Phase 4
	// task_append) or empty for err-tasks.
	Subject string `json:"subject,omitempty"`
	// Errored is true when the task carries
	// `errored: true` in frontmatter (Phase 5.B err-task).
	Errored bool `json:"errored,omitempty"`
	// DedupKey is the rendered per-pattern dedup key (Phase
	// 5.A); empty when no dedup applied.
	DedupKey string `json:"dedup_key,omitempty"`
	// CreatedAt is the frontmatter `created_at` parsed as
	// RFC3339. Zero value when the field is missing /
	// malformed.
	CreatedAt time.Time `json:"created_at"`
	// Path is the absolute file path on disk (operator-
	// inspection convenience; the HTTP wire surface elides
	// this by default).
	Path string `json:"-"`
}

// Task is the full task shape returned by Load. Includes the
// summary fields + the raw body (post-frontmatter) so the
// caller can render or parse sections as needed.
type Task struct {
	TaskSummary
	// Body is the markdown body after the frontmatter
	// closing `---`, verbatim. Includes section headers +
	// content. Empty when the file has no body.
	Body string `json:"body"`
}

// ListOptions controls the List filter shape. Zero-value
// returns every task; setting a field narrows the result
// per the comment.
type ListOptions struct {
	// Errored, when set, filters by the `errored` frontmatter
	// field. *true → only err-tasks; *false → only normal
	// tasks; nil → no filter (both).
	Errored *bool
}

// Reader walks a vault root's tasks/ directory and serves
// per-task reads. Construct via NewReader; safe for
// concurrent use (each operation re-reads the filesystem;
// no internal caching in v1).
type Reader struct {
	tasksDir string
}

// NewReader returns a Reader rooted at `<vaultRoot>/tasks/`.
// vaultRoot doesn't have to exist at construction time —
// List on a missing directory returns nil + nil (operator
// hasn't authored any workflows yet).
func NewReader(vaultRoot string) *Reader {
	return &Reader{tasksDir: filepath.Join(vaultRoot, "tasks")}
}

// ErrTaskNotFound is the typed sentinel Load returns when
// the id doesn't resolve to a file in the tasks/ directory.
// HTTP handlers translate this to 404.
var ErrTaskNotFound = errors.New("tasks: not found")

// List enumerates tasks under the configured tasks/
// directory, applying opts.Errored when set. Sorted by ID
// (== file basename) for deterministic output.
func (r *Reader) List(opts ListOptions) ([]TaskSummary, error) {
	entries, err := os.ReadDir(r.tasksDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tasks dir %q: %w", r.tasksDir, err)
	}

	var out []TaskSummary
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(r.tasksDir, e.Name())
		s, err := readSummary(path)
		if err != nil {
			// A single malformed file shouldn't block the
			// list; skip + continue. Future iteration could
			// surface a slog Warn here once the reader has
			// a logger threaded in.
			continue
		}
		if opts.Errored != nil && s.Errored != *opts.Errored {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Load returns the task at id (file basename without `.md`)
// with its summary fields + full body. Returns ErrTaskNotFound
// when no file matches; any other I/O error wraps cleanly.
func (r *Reader) Load(id string) (*Task, error) {
	if strings.ContainsAny(id, "/\\") {
		return nil, fmt.Errorf("tasks: id %q contains path separator", id)
	}
	path := filepath.Join(r.tasksDir, id+".md")
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
		}
		return nil, fmt.Errorf("read task file %q: %w", path, err)
	}
	fm, rest, err := splitFrontmatter(body)
	if err != nil {
		return nil, fmt.Errorf("parse task frontmatter %q: %w", path, err)
	}
	s := summaryFromFrontmatter(id, path, fm)
	return &Task{TaskSummary: s, Body: rest}, nil
}

// readSummary loads + parses a single task file's
// frontmatter, returning the summary view (no body).
// Internal helper for List.
func readSummary(path string) (TaskSummary, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return TaskSummary{}, err
	}
	fm, _, err := splitFrontmatter(body)
	if err != nil {
		return TaskSummary{}, err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".md")
	return summaryFromFrontmatter(id, path, fm), nil
}

// taskFrontmatter mirrors the on-disk YAML shape — both the
// regular-task fields (Phase 4.A + 5.A) and the err-task
// extension field (Phase 5.B).
type taskFrontmatter struct {
	Kind      string    `yaml:"kind"`
	Workflow  string    `yaml:"workflow"`
	Subject   string    `yaml:"subject,omitempty"`
	DedupKey  string    `yaml:"dedup_key,omitempty"`
	Errored   bool      `yaml:"errored,omitempty"`
	CreatedAt time.Time `yaml:"created_at,omitempty"`
}

// splitFrontmatter peels the leading `---\n...---\n` YAML
// frontmatter off body, returning (parsed, remaining). Body
// without a frontmatter block returns (zero, body, nil) — a
// task file without frontmatter is treated as bodyless-shape
// for graceful degradation.
func splitFrontmatter(body []byte) (taskFrontmatter, string, error) {
	const opener = "---\n"
	const closer = "\n---\n"
	if !strings.HasPrefix(string(body), opener) {
		return taskFrontmatter{}, string(body), nil
	}
	rest := string(body)[len(opener):]
	idx := strings.Index(rest, closer)
	if idx < 0 {
		// Unterminated frontmatter — treat as bodyless.
		return taskFrontmatter{}, string(body), nil
	}
	yamlBlock := rest[:idx]
	postBody := rest[idx+len(closer):]
	var fm taskFrontmatter
	dec := yaml.NewDecoder(strings.NewReader(yamlBlock))
	dec.KnownFields(false)
	if err := dec.Decode(&fm); err != nil {
		return taskFrontmatter{}, "", err
	}
	return fm, postBody, nil
}

// summaryFromFrontmatter builds the TaskSummary projection
// from the parsed frontmatter + path-derived id.
func summaryFromFrontmatter(id, path string, fm taskFrontmatter) TaskSummary {
	return TaskSummary{
		ID:        id,
		Workflow:  fm.Workflow,
		Subject:   fm.Subject,
		Errored:   fm.Errored,
		DedupKey:  fm.DedupKey,
		CreatedAt: fm.CreatedAt,
		Path:      path,
	}
}

