package wikipedia

import (
	"reflect"
	"testing"
)

// Test_notationsFor pins the per-article notation set yaad-wikipedia
// emits for the entity_notations cache (per yaad-index the source issue
// a prior PRv2). Always includes the originating input first; derives
// canonical URL, mobile-subdomain URL, and shorthand forms.
// Duplicates dedupe but preserve input-first ordering.
func Test_notationsFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		input string
		canonicalURL string
		lang string
		humanTitle string
		escapedTitle string
		want []string
	}{
		{
			name: "canonical URL input",
			input: "https://en.wikipedia.org/wiki/Susanna_Clarke",
			canonicalURL: "https://en.wikipedia.org/wiki/Susanna_Clarke",
			lang: "en",
			humanTitle: "Susanna Clarke",
			escapedTitle: "Susanna_Clarke",
			want: []string{
				"https://en.wikipedia.org/wiki/Susanna_Clarke",
				"https://en.m.wikipedia.org/wiki/Susanna_Clarke",
				"wikipedia: Susanna Clarke",
			},
		},
		{
			name: "shorthand input",
			input: "wikipedia: Susanna Clarke",
			canonicalURL: "https://en.wikipedia.org/wiki/Susanna_Clarke",
			lang: "en",
			humanTitle: "Susanna Clarke",
			escapedTitle: "Susanna_Clarke",
			want: []string{
				"wikipedia: Susanna Clarke",
				"https://en.wikipedia.org/wiki/Susanna_Clarke",
				"https://en.m.wikipedia.org/wiki/Susanna_Clarke",
			},
		},
		{
			name: "mobile-subdomain input lands as a fourth entry",
			input: "https://en.m.wikipedia.org/wiki/Susanna_Clarke",
			canonicalURL: "https://en.wikipedia.org/wiki/Susanna_Clarke",
			lang: "en",
			humanTitle: "Susanna Clarke",
			escapedTitle: "Susanna_Clarke",
			want: []string{
				"https://en.m.wikipedia.org/wiki/Susanna_Clarke",
				"https://en.wikipedia.org/wiki/Susanna_Clarke",
				"wikipedia: Susanna Clarke",
			},
		},
		{
			name: "non-english language",
			input: "https://de.wikipedia.org/wiki/Klingonisch",
			canonicalURL: "https://de.wikipedia.org/wiki/Klingonisch",
			lang: "de",
			humanTitle: "Klingonisch",
			escapedTitle: "Klingonisch",
			want: []string{
				"https://de.wikipedia.org/wiki/Klingonisch",
				"https://de.m.wikipedia.org/wiki/Klingonisch",
				"wikipedia: Klingonisch",
			},
		},
		{
			name: "empty lang defaults to en",
			input: "wikipedia: Iran",
			canonicalURL: "https://en.wikipedia.org/wiki/Iran",
			lang: "",
			humanTitle: "Iran",
			escapedTitle: "Iran",
			want: []string{
				"wikipedia: Iran",
				"https://en.wikipedia.org/wiki/Iran",
				"https://en.m.wikipedia.org/wiki/Iran",
			},
		},
		{
			name: "input equals canonical → no duplicate",
			input: "https://en.wikipedia.org/wiki/Iran",
			canonicalURL: "https://en.wikipedia.org/wiki/Iran",
			lang: "en",
			humanTitle: "Iran",
			escapedTitle: "Iran",
			want: []string{
				"https://en.wikipedia.org/wiki/Iran",
				"https://en.m.wikipedia.org/wiki/Iran",
				"wikipedia: Iran",
			},
		},
		{
			name: "URL-encoded title segment in derived URLs",
			input: "https://en.wikipedia.org/wiki/Go_(programming_language)",
			canonicalURL: "https://en.wikipedia.org/wiki/Go_(programming_language)",
			lang: "en",
			humanTitle: "Go (programming language)",
			escapedTitle: "Go_(programming_language)",
			want: []string{
				"https://en.wikipedia.org/wiki/Go_(programming_language)",
				"https://en.m.wikipedia.org/wiki/Go_(programming_language)",
				"wikipedia: Go (programming language)",
			},
		},
		{
			name: "missing humanTitle drops the shorthand",
			input: "https://en.wikipedia.org/wiki/Foo",
			canonicalURL: "https://en.wikipedia.org/wiki/Foo",
			lang: "en",
			humanTitle: "",
			escapedTitle: "Foo",
			want: []string{
				"https://en.wikipedia.org/wiki/Foo",
				"https://en.m.wikipedia.org/wiki/Foo",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := notationsFor(c.input, c.canonicalURL, c.lang, c.humanTitle, c.escapedTitle)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("notationsFor:\n want %#v\n got %#v", c.want, got)
			}
		})
	}
}
