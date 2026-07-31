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

func TestSplitDetailed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		limit    int
		text     string
		segments []string
		warnings int
	}{
		{name: "empty", limit: 3},
		{
			name: "packs complete phrases", limit: 4,
			text:     "Один. Два слова! Три.",
			segments: []string{"Один. Два слова! Три."},
		},
		{
			name: "starts new segment at limit", limit: 2,
			text:     "Один два. Три!",
			segments: []string{"Один два.", "Три!"},
		},
		{
			name: "preserves oversized phrase", limit: 3,
			text:     "один два три четыре пять шесть семь",
			segments: []string{"один два три четыре пять шесть семь"},
			warnings: 1,
		},
		{
			name: "oversized phrase flushes neighbors", limit: 3,
			text:     "До. один два три четыре пять. После.",
			segments: []string{"До.", "один два три четыре пять.", "После."},
			warnings: 1,
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
			result := segmenter.SplitDetailed(test.text)
			if !slices.Equal(result.Segments, test.segments) {
				t.Fatalf("segments = %q, want %q", result.Segments, test.segments)
			}
			if len(result.Warnings) != test.warnings {
				t.Fatalf("warnings = %#v, want %d", result.Warnings, test.warnings)
			}
		})
	}
}

func TestOversizedWarningContainsActionableMetadata(t *testing.T) {
	t.Parallel()
	segmenter, err := New(3)
	if err != nil {
		t.Fatal(err)
	}
	phrase := "один два три четыре пять шесть семь"
	result := segmenter.SplitDetailed(phrase)
	warning := result.Warnings[0]
	if warning.Phrase != phrase || warning.Words != 7 || warning.Limit != 3 {
		t.Fatalf("warning = %#v", warning)
	}
	if !strings.Contains(warning.Error(), "cannot fit") {
		t.Fatalf("warning text = %q", warning.Error())
	}
}

func TestSplitIntoUnits(t *testing.T) {
	t.Parallel()
	text := "Цена 3.14 рубля. «Правда?!» Да。 Ещё！ Всё？"
	want := []string{"Цена 3.14 рубля.", "«Правда?!»", "Да。", "Ещё！", "Всё？"}
	units := splitIntoUnits(text)
	got := make([]string, 0, len(units))
	for _, unit := range units {
		if unit.forceBoundary {
			t.Fatalf("unexpected forced boundary in %q", text)
		}
		got = append(got, unit.text)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("units = %q, want %q", got, want)
	}
}

func TestSceneBreakAlwaysForcesBoundaryAndIsRemoved(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"До. * * * После.",
		"До.\n *   *\t* \nПосле.",
		"До.* * *После.",
	} {
		segmenter, err := New(100)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := segmenter.Split(text), []string{"До.", "После."}; !slices.Equal(got, want) {
			t.Fatalf("Split(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestZeroValueUsesDefaultLimit(t *testing.T) {
	t.Parallel()
	var segmenter Segmenter
	if segmenter.MaxWords() != DefaultMaxWords {
		t.Fatalf("MaxWords() = %d", segmenter.MaxWords())
	}
	phrase := strings.TrimSpace(strings.Repeat("слово ", DefaultMaxWords+1))
	result := segmenter.SplitDetailed(phrase)
	if len(result.Segments) != 1 || len(result.Warnings) != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func FuzzSplitPreservesSourceText(f *testing.F) {
	f.Add("Короткая фраза. И ещё одна.")
	f.Add(strings.Repeat("слово ", DefaultMaxWords+1))
	f.Add("Цена 3.14. «Точно?!»")
	f.Add("До. * * * После.")

	f.Fuzz(func(t *testing.T, text string) {
		segmenter, err := New(7)
		if err != nil {
			t.Fatal(err)
		}
		result := segmenter.SplitDetailed(text)
		for _, segment := range result.Segments {
			if segment == "" {
				t.Fatal("empty segment")
			}
		}
		got := normalizeSpace(strings.Join(result.Segments, " "))
		want := normalizeWithoutSceneBreaks(text)
		if got != want {
			t.Fatalf("reconstructed text = %q, want %q", got, want)
		}
	})
}

func normalizeWithoutSceneBreaks(text string) string {
	var parts []string
	for _, line := range strings.Split(text, "\n") {
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
