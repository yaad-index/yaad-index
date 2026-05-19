// Single-file parser entry points. Workflow files are markdown:
// frontmatter holds metadata, body prose explains intent, and a
// single YAML code-fence inside the body holds the engine's
// structured rules. Parse splits the document, decodes each
// half, fuses them into one Workflow value, and runs schema
// validation.

package parser

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrFrontmatterMissing names the most common file-shape error:
// the file has no leading `---` block, so the parser can't
// recover the workflow name.
var ErrFrontmatterMissing = errors.New("workflow file: leading frontmatter block missing")

// ErrYAMLBodyMissing names the second most-common shape error:
// the file has frontmatter + prose but no ```yaml code-fence in
// the body. A workflow with no engine rules is a documentation
// stub, not a workflow.
var ErrYAMLBodyMissing = errors.New("workflow file: yaml code-fence body missing")

// ErrYAMLBodyDuplicated names the file-shape error where the
// body contains more than one ```yaml fence. The parser
// requires a single canonical block so operators can't
// accidentally split rules across fences (which the engine
// would then have to define merge semantics for).
var ErrYAMLBodyDuplicated = errors.New("workflow file: multiple yaml code-fences in body; expected exactly one")

// ParseFile parses the workflow file at path into a validated
// Workflow. Returns the parsed value on success; on error the
// error wraps the underlying cause + names the path so the
// loader can log a structured "file rejected" line.
func ParseFile(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("workflow file %q: read: %w", path, err)
	}
	wf, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("workflow file %q: %w", path, err)
	}
	return wf, nil
}

// Parse parses the raw bytes of a workflow file into a validated
// Workflow. Used directly by tests + by the loader's
// reload-on-mtime-bump path (where the bytes are already in
// memory).
func Parse(data []byte) (*Workflow, error) {
	frontmatter, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	yamlBody, err := extractYAMLFence(body)
	if err != nil {
		return nil, err
	}
	wf, err := decode(frontmatter, yamlBody)
	if err != nil {
		return nil, err
	}
	if err := Validate(wf); err != nil {
		return nil, err
	}
	return wf, nil
}

// frontmatterFenceRE matches the leading frontmatter delimiter.
// We accept either a Unix or Windows newline after the fence to
// tolerate operator-side editors that don't normalize.
var frontmatterFenceRE = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n?(.*)\z`)

// splitFrontmatter slices the input into (frontmatter, body) at
// the standard `---\n...\n---` envelope. Returns
// ErrFrontmatterMissing if the file doesn't open with `---`.
func splitFrontmatter(data []byte) ([]byte, []byte, error) {
	m := frontmatterFenceRE.FindSubmatch(data)
	if m == nil {
		return nil, nil, ErrFrontmatterMissing
	}
	return m[1], m[2], nil
}

// yamlFenceRE matches a ```yaml ... ``` code-fence in the body.
// Captures the fenced content; permissive about trailing
// whitespace on the opening fence line + the optional language
// tag (we require `yaml` to avoid grabbing arbitrary code
// blocks).
var yamlFenceRE = regexp.MustCompile("(?s)```yaml[ \t]*\r?\n(.*?)\r?\n```")

// extractYAMLFence returns the contents of the single ```yaml
// fence in body. Returns ErrYAMLBodyMissing if no yaml fence is
// present, ErrYAMLBodyDuplicated if there's more than one.
func extractYAMLFence(body []byte) ([]byte, error) {
	matches := yamlFenceRE.FindAllSubmatch(body, -1)
	if len(matches) == 0 {
		return nil, ErrYAMLBodyMissing
	}
	if len(matches) > 1 {
		return nil, ErrYAMLBodyDuplicated
	}
	return matches[0][1], nil
}

// frontmatterShape is the wire-shape struct for the markdown
// frontmatter half. Kept private; the public Workflow type is
// the fused product of frontmatterShape + bodyShape.
type frontmatterShape struct {
	Name    string `yaml:"name"`
	Version int    `yaml:"version"`
	Status  string `yaml:"status"`
}

// bodyShape is the wire-shape struct for the YAML code-fence
// half. Mirrors the ADR-0024 schema; the parser fuses this into
// Workflow + runs Validate.
type bodyShape struct {
	AllowedPlugins    []string         `yaml:"allowed_plugins"`
	Trigger           triggerShape     `yaml:"trigger"`
	Subject           string           `yaml:"subject"`
	Context           []ctxShape       `yaml:"context"`
	Condition         string           `yaml:"condition"`
	Dedup             *dedupShape      `yaml:"dedup"`
	Actions           []actionShape    `yaml:"actions"`
	AddableGaps       []string         `yaml:"addable_gaps"`
	AutoArchiveOnDone *bool            `yaml:"auto_archive_on_done"`
	CatchAll          bool             `yaml:"catch_all"`
}

type triggerShape struct {
	Type  string            `yaml:"type"`
	Match triggerMatchShape `yaml:"match"`
}

type triggerMatchShape struct {
	EdgeType   string `yaml:"edge_type"`
	TargetKind string `yaml:"target_kind"`
	Kind       string `yaml:"kind"`
	Gap        string `yaml:"gap"`
	Source     string `yaml:"source"`
}

type ctxShape struct {
	Name string `yaml:"name"`
	Via  string `yaml:"via"`
}

type dedupShape struct {
	Key    string `yaml:"key"`
	Policy string `yaml:"policy"`
}

// actionShape carries all four action primitives as optional
// fields; the parser enforces exactly-one-set at validate time.
// YAML's `mapping` shape naturally allows the operator to write
// `- task_append: {...}` for each list entry.
type actionShape struct {
	TaskAppend       *taskAppendShape       `yaml:"task_append"`
	AddNote          *addNoteShape          `yaml:"add_note"`
	PluginDispatch   *pluginDispatchShape   `yaml:"plugin_dispatch"`
	AddGap           *addGapShape           `yaml:"add_gap"`
	SetProperty      *setPropertyShape      `yaml:"set_property"`
	AddCanonicalEdge *addCanonicalEdgeShape `yaml:"add_canonical_edge"`
	ArchiveEntity    *archiveEntityShape    `yaml:"archive_entity"`
	ClaimEntity      *claimEntityShape      `yaml:"claim_entity"`
}

type claimEntityShape struct{}

type archiveEntityShape struct {
	Entity string `yaml:"entity"`
	Reason string `yaml:"reason"`
}

type addCanonicalEdgeShape struct {
	Source    string                    `yaml:"source"`
	EdgeType  string                    `yaml:"edge_type"`
	Target    addCanonicalEdgeTargetShape `yaml:"target"`
	Data      map[string]string         `yaml:"data"`
}

type addCanonicalEdgeTargetShape struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type taskAppendShape struct {
	Section          string `yaml:"section"`
	Content          string `yaml:"content"`
	IfAlreadyPresent string `yaml:"if_already_present"`
}

type addNoteShape struct {
	Target  string `yaml:"target"`
	Content string `yaml:"content"`
}

type pluginDispatchShape struct {
	Plugin         string         `yaml:"plugin"`
	Command        string         `yaml:"command"`
	Args           map[string]any `yaml:"args"`
	TimeoutSeconds int            `yaml:"timeout_seconds"`
}

type addGapShape struct {
	Entity       string            `yaml:"entity"`
	Gap          string            `yaml:"gap"`
	DataSchema   map[string]string `yaml:"data_schema"`
	Type         string            `yaml:"type"`
	Description  string            `yaml:"description"`
	FillStrategy string            `yaml:"fill_strategy"`
	Range        []int             `yaml:"range"`
	MaxLength    int               `yaml:"max_length"`
	Values       []string          `yaml:"values"`
	Kinds        []string          `yaml:"kinds"`
}

type setPropertyShape struct {
	Entity string            `yaml:"entity"`
	Fields map[string]string `yaml:"fields"`
}

// decode runs the two YAML decode passes (frontmatter + body)
// + fuses the results into a Workflow. KnownFields strict mode
// catches typo-shaped errors (unknown YAML keys) — operators
// who misspell a field get a clear error instead of silently
// dropping the value.
func decode(frontmatter, yamlBody []byte) (*Workflow, error) {
	var fm frontmatterShape
	dec := yaml.NewDecoder(bytes.NewReader(frontmatter))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil {
		return nil, fmt.Errorf("frontmatter yaml: %w", err)
	}

	var body bodyShape
	dec = yaml.NewDecoder(bytes.NewReader(yamlBody))
	dec.KnownFields(true)
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("body yaml: %w", err)
	}

	wf := &Workflow{
		Name:              strings.TrimSpace(fm.Name),
		Version:           fm.Version,
		Status:            strings.TrimSpace(fm.Status),
		AllowedPlugins:    body.AllowedPlugins,
		Trigger:           triggerFromShape(body.Trigger),
		Subject:           strings.TrimSpace(body.Subject),
		Context:           contextFromShape(body.Context),
		Condition:         strings.TrimSpace(body.Condition),
		Dedup:             dedupFromShape(body.Dedup),
		Actions:           actionsFromShape(body.Actions),
		AddableGaps:       body.AddableGaps,
		AutoArchiveOnDone: body.AutoArchiveOnDone,
		CatchAll:          body.CatchAll,
	}
	if wf.Version == 0 {
		wf.Version = 1
	}
	if wf.Status == "" {
		wf.Status = StatusActive
	}
	// Subject defaults to `entity.id` per ADR-0024 §"Workflow"
	// ("workflows that omit it default to entity.id"). Apply at
	// parse time so the engine consumer doesn't have to re-check
	// for empty + apply the same default — symmetric with the
	// version=1 / status=active defaults above. PR-77 review
	// note fold-in.
	if wf.Subject == "" {
		wf.Subject = "entity.id"
	}
	// task_append.if_already_present defaults to "skip" per
	// ADR-0024 §"Action-level match semantics". Apply at parse
	// time for the same symmetry reason. PR-77 review note
	// fold-in.
	for i := range wf.Actions {
		if wf.Actions[i].TaskAppend != nil && wf.Actions[i].TaskAppend.IfAlreadyPresent == "" {
			wf.Actions[i].TaskAppend.IfAlreadyPresent = IfAlreadyPresentSkip
		}
	}
	applyDedupDefaults(wf)
	return wf, nil
}

func triggerFromShape(t triggerShape) Trigger {
	return Trigger{
		Type: strings.TrimSpace(t.Type),
		Match: TriggerMatch{
			EdgeType:   strings.TrimSpace(t.Match.EdgeType),
			TargetKind: strings.TrimSpace(t.Match.TargetKind),
			Kind:       strings.TrimSpace(t.Match.Kind),
			Gap:        strings.TrimSpace(t.Match.Gap),
			Source:     strings.TrimSpace(t.Match.Source),
		},
	}
}

func contextFromShape(entries []ctxShape) []ContextBinding {
	if len(entries) == 0 {
		return nil
	}
	out := make([]ContextBinding, len(entries))
	for i, e := range entries {
		out[i] = ContextBinding{
			Name: strings.TrimSpace(e.Name),
			Via:  strings.TrimSpace(e.Via),
		}
	}
	return out
}

func dedupFromShape(d *dedupShape) Dedup {
	if d == nil {
		return Dedup{}
	}
	return Dedup{
		Key:    strings.TrimSpace(d.Key),
		Policy: strings.TrimSpace(d.Policy),
	}
}

// applyDedupDefaults stamps the per-pattern dedup defaults
// per ADR-0024 §"Per-pattern de-duplication":
//   - Policy defaults to "update".
//   - Key defaults to `entity.id` (the CEL-compatible portion
//     of the ADR's `workflow + entity.id` default; the engine
//     prefixes the workflow name engine-side so the rendered
//     key is namespaced across workflows without requiring the
//     CEL env to know its own workflow name).
//
// Called from decode AFTER other field-level defaults so the
// dedup defaults compose with the rest of the workflow shape.
func applyDedupDefaults(wf *Workflow) {
	if wf.Dedup.Policy == "" {
		wf.Dedup.Policy = DedupPolicyUpdate
	}
	if wf.Dedup.Key == "" {
		wf.Dedup.Key = "entity.id"
	}
}

func actionsFromShape(entries []actionShape) []Action {
	if len(entries) == 0 {
		return nil
	}
	out := make([]Action, len(entries))
	for i, e := range entries {
		out[i] = Action{
			TaskAppend:       taskAppendFromShape(e.TaskAppend),
			AddNote:          addNoteFromShape(e.AddNote),
			PluginDispatch:   pluginDispatchFromShape(e.PluginDispatch),
			AddGap:           addGapFromShape(e.AddGap),
			SetProperty:      setPropertyFromShape(e.SetProperty),
			AddCanonicalEdge: addCanonicalEdgeFromShape(e.AddCanonicalEdge),
			ArchiveEntity:    archiveEntityFromShape(e.ArchiveEntity),
			ClaimEntity:      claimEntityFromShape(e.ClaimEntity),
		}
	}
	return out
}

func taskAppendFromShape(s *taskAppendShape) *TaskAppendAction {
	if s == nil {
		return nil
	}
	return &TaskAppendAction{
		Section:          strings.TrimSpace(s.Section),
		Content:          s.Content,
		IfAlreadyPresent: strings.TrimSpace(s.IfAlreadyPresent),
	}
}

func addNoteFromShape(s *addNoteShape) *AddNoteAction {
	if s == nil {
		return nil
	}
	return &AddNoteAction{
		Target:  strings.TrimSpace(s.Target),
		Content: s.Content,
	}
}

func pluginDispatchFromShape(s *pluginDispatchShape) *PluginDispatchAction {
	if s == nil {
		return nil
	}
	return &PluginDispatchAction{
		Plugin:         strings.TrimSpace(s.Plugin),
		Command:        strings.TrimSpace(s.Command),
		Args:           s.Args,
		TimeoutSeconds: s.TimeoutSeconds,
	}
}

func addGapFromShape(s *addGapShape) *AddGapAction {
	if s == nil {
		return nil
	}
	var schema map[string]string
	if len(s.DataSchema) > 0 {
		schema = make(map[string]string, len(s.DataSchema))
		for k, v := range s.DataSchema {
			schema[strings.TrimSpace(k)] = v
		}
	}
	var kinds []string
	if len(s.Kinds) > 0 {
		kinds = make([]string, len(s.Kinds))
		for i, k := range s.Kinds {
			kinds[i] = strings.TrimSpace(k)
		}
	}
	var values []string
	if len(s.Values) > 0 {
		values = make([]string, len(s.Values))
		for i, v := range s.Values {
			values[i] = strings.TrimSpace(v)
		}
	}
	var rangePair []int
	if len(s.Range) > 0 {
		rangePair = make([]int, len(s.Range))
		copy(rangePair, s.Range)
	}
	return &AddGapAction{
		Entity:       strings.TrimSpace(s.Entity),
		Gap:          strings.TrimSpace(s.Gap),
		DataSchema:   schema,
		Type:         strings.TrimSpace(s.Type),
		Description:  s.Description,
		FillStrategy: strings.TrimSpace(s.FillStrategy),
		Range:        rangePair,
		MaxLength:    s.MaxLength,
		Values:       values,
		Kinds:        kinds,
	}
}

func addCanonicalEdgeFromShape(s *addCanonicalEdgeShape) *AddCanonicalEdgeAction {
	if s == nil {
		return nil
	}
	var data map[string]string
	if len(s.Data) > 0 {
		data = make(map[string]string, len(s.Data))
		for k, v := range s.Data {
			data[strings.TrimSpace(k)] = v
		}
	}
	return &AddCanonicalEdgeAction{
		Source:     strings.TrimSpace(s.Source),
		EdgeType:   strings.TrimSpace(s.EdgeType),
		TargetKind: strings.TrimSpace(s.Target.Kind),
		TargetName: strings.TrimSpace(s.Target.Name),
		Data:       data,
	}
}

func setPropertyFromShape(s *setPropertyShape) *SetPropertyAction {
	if s == nil {
		return nil
	}
	fields := make(map[string]string, len(s.Fields))
	for k, v := range s.Fields {
		fields[strings.TrimSpace(k)] = v
	}
	return &SetPropertyAction{
		Entity: strings.TrimSpace(s.Entity),
		Fields: fields,
	}
}

func archiveEntityFromShape(s *archiveEntityShape) *ArchiveEntityAction {
	if s == nil {
		return nil
	}
	return &ArchiveEntityAction{
		Entity: strings.TrimSpace(s.Entity),
		Reason: strings.TrimSpace(s.Reason),
	}
}

func claimEntityFromShape(s *claimEntityShape) *ClaimEntityAction {
	if s == nil {
		return nil
	}
	return &ClaimEntityAction{}
}
