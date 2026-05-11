package api

import (
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EnvSnippetMaxChars is the env var operators set to cap the per-result
// snippet length on /v1/search. Default DefaultSnippetMaxChars (200) is
// enough for a one-sentence preview without bloating list responses.
const EnvSnippetMaxChars = "YAAD_INDEX_SNIPPET_MAX_CHARS"

// DefaultSnippetMaxChars caps the snippet length when the env override
// is unset / unparseable. Picked at 200 because: agent-filled summaries
// are typically ~100-300 char prose; 200 fits one or two sentences
// cleanly without truncating the typical case mid-thought.
const DefaultSnippetMaxChars = 200

// stringField returns data[key] as a string, treating non-string /
// missing values as empty. Tolerates the float64 / json.Number quirks
// from json.Unmarshal-into-map[string]any by NOT trying to coerce —
// snippets are prose, not numbers.
func stringField(data map[string]any, key string) string {
	v, ok := data[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// truncate caps a snippet to maxChars by rune count (so multi-byte
// UTF-8 isn't sliced mid-codepoint). Adds an ellipsis "…" suffix when
// the input was actually truncated, so the wire shape signals the
// boundary to clients without their needing the original length.
func truncate(s string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	// Walk runes, stop once we've taken maxChars. Tracking byte
	// offsets keeps the slice operation O(maxChars) and avoids the
	// extra allocation a []rune detour would cost.
	cut := 0
	count := 0
	for i := range s {
		if count == maxChars {
			cut = i
			break
		}
		count++
	}
	if cut == 0 {
		return s
	}
	return strings.TrimRight(s[:cut], " ") + "…"
}

func readSnippetMaxChars() int {
	if raw := os.Getenv(EnvSnippetMaxChars); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return DefaultSnippetMaxChars
}
