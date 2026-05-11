package slug

import "testing"

// TestSlug exercises the documented behaviors of the thin wrapper:
// the slug-side normalization that gosimple/slug provides
// (lowercase, hyphenation, transliteration). The wrapper itself
// adds no logic — these tests are integration-style assertions
// that pin the underlying library's behavior so a future swap or
// version bump that changes output surfaces in CI.
//
// Disambig stripping (year-suffix, parens-disambig, BGG series-
// separator) is NOT tested here — that's plugin-side per ADR-0021.
// Tests covering plugin-owned canonical-name production live in
// the plugin repos (yaad-bgg, yaad-wikipedia).
func TestSlug(t *testing.T) {
	cases := []struct {
		name string
		in string
		want string
	}{
		// ASCII normalization.
		{"empty", "", ""},
		{"lowercase_passthrough", "hello", "hello"},
		{"already_slug", "hello-world", "hello-world"},
		{"uppercase_to_lowercase", "Hello", "hello"},
		{"mixed_case_with_space", "Hello World", "hello-world"},

		// Whitespace + punctuation hyphenation.
		{"single_space", "hello world", "hello-world"},
		{"multi_space", "hello world", "hello-world"},
		{"tab", "hello\tworld", "hello-world"},
		{"colon", "Brass: Birmingham", "brass-birmingham"},
		{"comma", "Smith, John", "smith-john"},
		{"slash", "yes/no", "yes-no"},
		{"apostrophe", "it's a wonderful life", "its-a-wonderful-life"},
		{"underscore", "hello_world", "hello_world"},

		// Numbers preserved.
		{"with_numbers", "version 2 release", "version-2-release"},
		{"all_numbers", "12345", "12345"},

		// Hyphen handling.
		{"leading_punct", "!!!hello", "hello"},
		{"trailing_punct", "hello!!!", "hello"},
		{"surrounding_hyphens", "-hello-", "hello"},

		// Latin diacritics — gosimple/slug transliterates via
		// unidecode tables.
		{"umlaut", "Über", "uber"},
		{"acute", "café", "cafe"},
		{"tilde", "São Paulo", "sao-paulo"},
		{"cedilla", "façade", "facade"},
		{"o_with_stroke", "Søren", "soren"},

		// Non-Latin scripts — unidecode produces a romanized form.
		// Pin the current library output so a change is visible.
		{"cyrillic", "Чехов", "chekhov"},
		{"chinese", "三国杀", "san-guo-sha"},
		{"emoji_drop", "Game 🎲", "game"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Slug(tc.in)
			if got != tc.want {
				t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlug_Determinism asserts the acceptance criterion "identical
// inputs always produce identical outputs."
func TestSlug_Determinism(t *testing.T) {
	inputs := []string{
		"Hello World",
		"Über das Café",
		"Чехов",
		"三国杀",
		"!!!---!!!",
		"",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			a, b := Slug(in), Slug(in)
			if a != b {
				t.Errorf("Slug(%q) not deterministic: %q vs %q", in, a, b)
			}
		})
	}
}
