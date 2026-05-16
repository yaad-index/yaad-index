// Package template implements the workflow engine's
// string-template renderer used for `subject`, `dedup.key`, and
// per-action template fields per ADR-0024 §"Workflow". A
// template source is one of two shapes:
//
//   - **Bare CEL** — the entire source string is a single CEL
//     expression returning a string (or any type the decision
//     package stringifies). Existing operator-authored
//     workflows like `subject: entity.id` are bare CEL.
//   - **Mustache** — the source contains one or more
//     `{{ expr }}` interpolation segments surrounded by literal
//     text (the ADR's canonical shape:
//     `'{{ entity.name }} ({{ entity.year }})'`). Each segment
//     is a CEL expression; the surrounding text is rendered
//     verbatim.
//
// The mode is selected by source contents — sources containing
// `{{` enter mustache mode, everything else compiles as bare
// CEL. Both shapes produce a Template with the same Render
// surface so engine + runner code stays uniform.
//
// **Why a layer over decision.Compile.** decision.Compile
// accepts a single CEL expression — workable for `condition`
// and `context.via` (single-value evaluations), but not for
// templates that mix literal text with multiple expressions.
// Wrapping per-segment compile + concat-render in this package
// keeps decision focused on expression eval and lets the engine
// pre-compile every template at workflow-register time so
// malformed expressions surface before any event fires.
//
// **Limits in v1.** The mustache scanner uses non-greedy
// `{{...}}` matching — the first `}}` after `{{` closes the
// segment. A CEL string literal containing `}}` inside a
// mustache segment (e.g. `{{ "}}" }}`) would close
// prematurely. Operators that need literal `}}` characters in
// surface text use bare-CEL mode with an explicit CEL string
// literal (e.g. `subject: "'foo }} bar'"`). Future iteration
// can teach the scanner about CEL string boundaries if the
// limit bites in practice.

package template

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/yaad-index/yaad-index/internal/workflow/decision"
)

// Template is a parsed + compiled mustache template. Construct
// via Compile; render against an activation via Render.
//
// Each Template is bound to the decision.Evaluator passed at
// Compile time — the CEL programs reference the Evaluator's
// variable scope (entity / edge / context bindings). Rendering
// a Template against an Evaluator other than the one it was
// compiled with is unsupported.
type Template struct {
	// Raw is the original source string, retained for error
	// messages + debugging.
	Raw      string
	segments []segment
}

// segment is one piece of a parsed template — either a literal
// text run or a compiled CEL expression. Exactly one of literal
// / program is non-zero per segment.
type segment struct {
	literal string
	program *decision.Program
	// exprSrc retains the CEL source for error messages when
	// program eval fails at runtime.
	exprSrc string
}

// Compile parses src and returns a Template ready for
// concurrent-safe Render calls. The mode is chosen by source
// contents: sources containing `{{` enter mustache mode (parse
// + per-segment CEL compile); all other non-empty sources
// compile as a single bare CEL expression. An empty src
// produces a no-op template that renders to "".
//
// Mustache-mode parse rejects empty expressions (`{{ }}`) and
// unbalanced braces. CEL-mode compile failures propagate
// through ev.Compile.
func Compile(src string, ev *decision.Evaluator) (*Template, error) {
	if ev == nil {
		return nil, fmt.Errorf("template: Compile requires a non-nil decision.Evaluator")
	}
	if src == "" {
		return &Template{Raw: src}, nil
	}
	if !strings.Contains(src, "{{") {
		// Bare-CEL mode: the entire source is a single
		// string-returning CEL expression. Preserves the
		// operator-authored shape `subject: entity.id` /
		// `target: prior.id` where the YAML value is a CEL
		// expression rather than a mustache template. A stray
		// `}}` with no opening `{{` is rejected here so the
		// failure mode matches mustache-mode strictness.
		if strings.Contains(src, "}}") {
			return nil, fmt.Errorf("template: parse %q: unmatched %q", src, "}}")
		}
		prog, err := ev.Compile(src, "string")
		if err != nil {
			return nil, fmt.Errorf("template: compile %q: %w", src, err)
		}
		return &Template{
			Raw:      src,
			segments: []segment{{program: prog, exprSrc: src}},
		}, nil
	}
	parts, err := parseMustache(src)
	if err != nil {
		return nil, fmt.Errorf("template: parse %q: %w", src, err)
	}
	tpl := &Template{Raw: src}
	for _, p := range parts {
		if !p.isCEL {
			tpl.segments = append(tpl.segments, segment{literal: p.text})
			continue
		}
		prog, err := ev.Compile(p.text, "string")
		if err != nil {
			return nil, fmt.Errorf("template: compile segment %q in %q: %w", p.text, src, err)
		}
		tpl.segments = append(tpl.segments, segment{program: prog, exprSrc: p.text})
	}
	return tpl, nil
}

// Render evaluates the template against act and returns the
// concatenated literal-segments + CEL-eval-results. The
// returned decision.Result aggregates every CEL segment's
// MissingRefs (deduplicated by id + sorted) so the engine can
// surface them as task notes per ADR-0024 §"Missing-reference
// handling".
//
// On a per-segment eval error, Render returns the wrapped
// error immediately; partial output is discarded.
func (t *Template) Render(ctx context.Context, act decision.Activation) (string, decision.Result, error) {
	var b strings.Builder
	b.Grow(len(t.Raw))
	var refs []decision.MissingRef
	for _, s := range t.segments {
		if s.program == nil {
			b.WriteString(s.literal)
			continue
		}
		val, res, err := s.program.EvalString(ctx, act)
		if err != nil {
			return "", decision.Result{}, fmt.Errorf("template: render segment %q in %q: %w", s.exprSrc, t.Raw, err)
		}
		b.WriteString(val)
		refs = append(refs, res.MissingRefs...)
	}
	return b.String(), dedupRefs(refs), nil
}

// dedupRefs collapses duplicate-by-id MissingRef entries +
// sorts the result for deterministic output. Mirrors the
// engine's dedupMissingRefs helper but lives here so the
// template package stays independent.
func dedupRefs(refs []decision.MissingRef) decision.Result {
	if len(refs) == 0 {
		return decision.Result{}
	}
	seen := make(map[string]struct{}, len(refs))
	out := make([]decision.MissingRef, 0, len(refs))
	for _, r := range refs {
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return decision.Result{MissingRefs: out}
}

// mustachePart is one parsed segment from parseMustache —
// either a literal text run or an extracted CEL expression
// source.
type mustachePart struct {
	text  string
	isCEL bool
}

// parseMustache walks src and yields the alternating literal
// and CEL-expression segments. Empty CEL segments (`{{ }}`)
// and unbalanced braces produce a typed error rather than
// silent acceptance — workflow authors that want literal
// `{{` / `}}` characters in surface text use a CEL string
// literal segment (e.g. `{{ "{{" }}`).
func parseMustache(src string) ([]mustachePart, error) {
	var out []mustachePart
	for i := 0; i < len(src); {
		open := strings.Index(src[i:], "{{")
		if open < 0 {
			// No more expressions; rest is literal. Reject any
			// stray `}}` to keep the grammar unambiguous.
			rest := src[i:]
			if idx := strings.Index(rest, "}}"); idx >= 0 {
				return nil, fmt.Errorf("unmatched %q at offset %d", "}}", i+idx)
			}
			if rest != "" {
				out = append(out, mustachePart{text: rest})
			}
			break
		}
		// Emit literal prefix (which itself must not contain `}}`).
		prefix := src[i : i+open]
		if idx := strings.Index(prefix, "}}"); idx >= 0 {
			return nil, fmt.Errorf("unmatched %q at offset %d", "}}", i+idx)
		}
		if prefix != "" {
			out = append(out, mustachePart{text: prefix})
		}
		// Move past the opening `{{` and locate the closing `}}`.
		exprStart := i + open + 2
		close := strings.Index(src[exprStart:], "}}")
		if close < 0 {
			return nil, fmt.Errorf("unmatched %q starting at offset %d", "{{", i+open)
		}
		expr := strings.TrimSpace(src[exprStart : exprStart+close])
		if expr == "" {
			return nil, fmt.Errorf("empty expression in %q at offset %d", "{{ }}", i+open)
		}
		out = append(out, mustachePart{text: expr, isCEL: true})
		i = exprStart + close + 2
	}
	return out, nil
}
