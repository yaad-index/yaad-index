package decision

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// regex_capture(text, pattern, group_index) -> string is a workflow
// CEL helper for extracting a capture group from text. Returns the
// match for the given group (0 = whole match) or "" when there is
// no match or the group index is out of range. Pattern compilation
// uses Go's standard regexp/RE2 syntax.
//
// Compiled regexes are cached process-wide by pattern string;
// repeated evals with the same pattern (the common case — patterns
// are typically string literals baked into the workflow YAML) hit
// the cache.
//
// Literal-pattern validation runs at Compile time
// (validateLiteralRegexCaptures) so a malformed pattern in the
// workflow source surfaces when the workflow is registered rather
// than at fire time. Patterns computed at runtime cannot be
// pre-validated; those return "" on bad-pattern at eval time.

const regexCaptureFuncName = "regex_capture"

func regexCaptureFunction() cel.EnvOption {
	return cel.Function(regexCaptureFuncName,
		cel.Overload(
			"regex_capture_string_string_int_string",
			[]*cel.Type{cel.StringType, cel.StringType, cel.IntType},
			cel.StringType,
			cel.FunctionBinding(regexCaptureImpl),
		),
	)
}

func regexCaptureImpl(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("regex_capture: want 3 args, got %d", len(args))
	}
	text, ok := args[0].Value().(string)
	if !ok {
		return types.NewErr("regex_capture: arg 1 must be string, got %T", args[0].Value())
	}
	pattern, ok := args[1].Value().(string)
	if !ok {
		return types.NewErr("regex_capture: arg 2 must be string, got %T", args[1].Value())
	}
	groupIdx, ok := args[2].Value().(int64)
	if !ok {
		return types.NewErr("regex_capture: arg 3 must be int, got %T", args[2].Value())
	}
	re, err := compiledRegex(pattern)
	if err != nil {
		return types.String("")
	}
	match := re.FindStringSubmatch(text)
	if match == nil {
		return types.String("")
	}
	if groupIdx < 0 || int(groupIdx) >= len(match) {
		return types.String("")
	}
	return types.String(match[int(groupIdx)])
}

type regexEntry struct {
	re  *regexp.Regexp
	err error
}

var regexCache sync.Map // pattern (string) -> *regexEntry

func compiledRegex(pattern string) (*regexp.Regexp, error) {
	if v, ok := regexCache.Load(pattern); ok {
		e := v.(*regexEntry)
		return e.re, e.err
	}
	re, err := regexp.Compile(pattern)
	regexCache.Store(pattern, &regexEntry{re: re, err: err})
	return re, err
}

// validateLiteralRegexCaptures walks the checked AST for
// regex_capture(_, literal, _) calls and compiles each literal
// pattern eagerly so a bad pattern fails Compile rather than fire.
// Non-literal patterns (computed from entity/edge fields, bindings)
// can only be validated at eval time and return "" on failure.
func validateLiteralRegexCaptures(checked *cel.Ast, expr string) error {
	native := checked.NativeRep()
	nav := celast.NavigateAST(native)
	for _, m := range celast.MatchDescendants(nav, celast.FunctionMatcher(regexCaptureFuncName)) {
		call := m.AsCall()
		args := call.Args()
		if len(args) < 2 {
			continue
		}
		patternArg := args[1]
		if patternArg.Kind() != celast.LiteralKind {
			continue
		}
		lit, ok := patternArg.AsLiteral().Value().(string)
		if !ok {
			continue
		}
		if _, err := regexp.Compile(lit); err != nil {
			return fmt.Errorf("decision: parse %q: regex_capture pattern %q invalid: %w", expr, lit, err)
		}
	}
	return nil
}
