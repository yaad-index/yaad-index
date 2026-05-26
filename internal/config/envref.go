package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ErrUnresolvedEnvReference signals an instance env value that
// names a `${NAME}` reference whose variable isn't present in
// the daemon's process environment. The caller (buildInstanceEnv,
// startup-validation pass) surfaces the error with plugin +
// instance context so the operator sees which secret store
// entry is missing.
var ErrUnresolvedEnvReference = errors.New("config: unresolved env reference")

// envRefPattern matches the `${NAME}` reference shape per #256.
// Strict syntax — `${...}` only, no `$NAME` shell shorthand, no
// `${VAR:-default}` fallback, no escape sequence. Future v1.x
// can grow the surface if operator-facing patterns demand it;
// the conservative v1 shape keeps the parser predictable + free
// of corner cases (a literal `$` outside `${...}` passes through
// verbatim).
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnvReferences walks `value` and substitutes every
// `${NAME}` reference with the corresponding value from the
// daemon's process environment (os.LookupEnv source — populated
// by systemd's EnvironmentFile directive or any other process-
// env hook). Returns the expanded string + an `emptyRefs`
// slice naming any references whose env var resolved to an
// empty string (caller decides whether to warn vs proceed).
//
// Missing references — env var entirely absent from
// `os.LookupEnv` — return a wrapped ErrUnresolvedEnvReference
// naming the offending variable so the caller can surface
// plugin + instance context in the operator-visible error.
//
// Strict syntax per #256: only `${NAME}` is expanded. Bare
// `$NAME` and `${VAR:-default}` are deliberately out of scope;
// the former collides with literal `$` in tokens (PATs / API
// keys commonly carry `$`), the latter introduces shell-
// semantics ambiguity. Future revisions may extend.
func ExpandEnvReferences(value string) (expanded string, emptyRefs []string, err error) {
	matches := envRefPattern.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value, nil, nil
	}
	var out strings.Builder
	last := 0
	for _, m := range matches {
		// m = [matchStart matchEnd nameStart nameEnd]
		matchStart, matchEnd := m[0], m[1]
		nameStart, nameEnd := m[2], m[3]
		name := value[nameStart:nameEnd]
		resolved, present := os.LookupEnv(name)
		if !present {
			return "", nil, fmt.Errorf("%w: ${%s}", ErrUnresolvedEnvReference, name)
		}
		if resolved == "" {
			emptyRefs = append(emptyRefs, name)
		}
		out.WriteString(value[last:matchStart])
		out.WriteString(resolved)
		last = matchEnd
	}
	out.WriteString(value[last:])
	return out.String(), emptyRefs, nil
}
