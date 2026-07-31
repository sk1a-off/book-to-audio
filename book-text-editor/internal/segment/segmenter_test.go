package segment

import (
	"slices"
	"strings"
	"testing"
)

func TestNewRejectsInvalidLimit(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{-1, 0} {
		if _, err := New(limit); err == nil {
			t.Fatalf("New(%d) returned nil error", limit)
		}
	}
}

func TestZeroValueUsesDefaultLimit(t *testing.T) {
	t.Parallel()

	var segmenter Segmenter
	if got := segmenter.MaxWords(); got != DefaultMaxWords {
		t.Fatalf("MaxWords() = %d, want %d", got, DefaultMaxWords)
	}
}

func TestSplit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		limit    int
		text     string
		expected []string
	}{
		{
			name:  "empty text",
			limit: 3,
		},
		{
			name:     "packs complete phrases",
			limit:    4,
			text:     "Один. Два слова! Три.",
			expected: []string{"Один. Два слова! Три."},
		},
		{
			name:     "starts a segment at the limit",
			limit:    2,
			text:     "Один два. Три!",
			expected: []string{"Один два.", "Три!"},
		},
		{
			name:     "uses line as a phrase boundary",
			limit:    2,
			text:     "Один два\nТри четыре",
			expected: []string{"Один два", "Три четыре"},
		},
		{
			name:     "splits an oversized phrase",
			limit:    3,
			text:     "один два три четыре пять шесть семь",
			expected: []string{"один два три", "четыре пять шесть", "семь"},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			segmenter, err := New(test.limit)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			actual := segmenter.Split(test.text)
			if !slices.Equal(actual, test.expected) {
				t.Fatalf("Split() = %q, want %q", actual, test.expected)
			}
		})
	}
}

func TestSplitIntoUnits(t *testing.T) {
	t.Parallel()

	text := "Цена 3.14 рубля. «Правда?!» Да。 Ещё！ Всё？"
	expected := []string{
		"Цена 3.14 рубля.",
		"«Правда?!»",
		"Да。",
		"Ещё！",
		"Всё？",
	}

	units := splitIntoUnits(text)
	actual := make([]string, 0, len(units))
	for _, unit := range units {
		if unit.forceBoundary {
			t.Fatalf("splitIntoUnits() unexpectedly forced segment %q", unit.text)
		}
		actual = append(actual, unit.text)
	}

	if !slices.Equal(actual, expected) {
		t.Fatalf("splitIntoUnits() = %q, want %q", actual, expected)
	}
}

func TestSceneBreakAlwaysForcesBoundaryAndIsRemoved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		expected []string
	}{
		{
			name:     "between phrases",
			text:     "До разделителя. * * * После разделителя.",
			expected: []string{"До разделителя.", "После разделителя."},
		},
		{
			name:     "separate line with irregular whitespace",
			text:     "До разделителя.\n *   *\t* \nПосле разделителя.",
			expected: []string{"До разделителя.", "После разделителя."},
		},
		{
			name:     "embedded without surrounding whitespace",
			text:     "До разделителя.* * *После разделителя.",
			expected: []string{"До разделителя.", "После разделителя."},
		},
		{
			name:     "at the beginning",
			text:     "* * * После разделителя.",
			expected: []string{"После разделителя."},
		},
		{
			name:     "at the end",
			text:     "До разделителя. * * *",
			expected: []string{"До разделителя."},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			segmenter, err := New(100)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			if actual := segmenter.Split(test.text); !slices.Equal(actual, test.expected) {
				t.Fatalf("Split() = %q, want %q", actual, test.expected)
			}
		})
	}
}

func TestSceneBreakWithoutTextProducesNoSegment(t *testing.T) {
	t.Parallel()

	segmenter, err := New(1)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if actual := segmenter.Split(sceneBreak); len(actual) != 0 {
		t.Fatalf("Split() = %q, want no segments", actual)
	}
}

func TestSplitAtDefaultLimitBoundary(t *testing.T) {
	t.Parallel()

	segmenter := Segmenter{}
	exactlyAtLimit := strings.TrimSpace(strings.Repeat("слово ", DefaultMaxWords))
	overLimit := strings.TrimSpace(strings.Repeat("слово ", DefaultMaxWords+1))

	if actual := segmenter.Split(exactlyAtLimit); len(actual) != 1 {
		t.Fatalf("Split(%d words) returned %d segments, want 1", DefaultMaxWords, len(actual))
	}

	actual := segmenter.Split(overLimit)
	if len(actual) != 2 {
		t.Fatalf("Split(%d words) returned %d segments, want 2", DefaultMaxWords+1, len(actual))
	}
	for _, result := range actual {
		if words := countWords(result); words > DefaultMaxWords {
			t.Fatalf("segment contains %d words, limit is %d", words, DefaultMaxWords)
		}
	}
}

func FuzzSplitRespectsWordLimit(f *testing.F) {
	f.Add("Короткая фраза. И ещё одна.")
	f.Add(strings.Repeat("слово ", DefaultMaxWords+1))
	f.Add("Цена 3.14. «Точно?!»")
	f.Add("До. * * * После.")

	f.Fuzz(func(t *testing.T, text string) {
		const limit = 7

		segmenter, err := New(limit)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}

		results := segmenter.Split(text)
		for _, result := range results {
			if result == "" {
				t.Fatal("Split() returned an empty segment")
			}
			if words := countWords(result); words > limit {
				t.Fatalf("segment contains %d words, limit is %d: %q", words, limit, result)
			}
		}

		actualText := normalizeSpace(strings.Join(results, " "))
		expectedText := normalizeWithoutSceneBreaks(text)
		if actualText != expectedText {
			t.Fatalf("reconstructed text = %q, want %q", actualText, expectedText)
		}
	})
}

func normalizeWithoutSceneBreaks(text string) string {
	text = string([]rune(text))
	var parts []string

	for line := range strings.Lines(text) {
		line = normalizeSpace(line)
		for {
			before, after, found := strings.Cut(line, sceneBreak)
			if before = normalizeSpace(before); before != "" {
				parts = append(parts, before)
			}
			if !found {
				break
			}
			line = after
		}
	}

	return normalizeSpace(strings.Join(parts, " "))
}
