package segment

import (
	"slices"
	"strings"
	"testing"
)

func TestSplitPacksCompletePhrases(t *testing.T) {
	segmenter, err := New(4)
	if err != nil {
		t.Fatal(err)
	}
	got := segmenter.Split("Один. Два слова! Три.")
	want := []string{"Один. Два слова! Три."}
	if !slices.Equal(got, want) {
		t.Fatalf("Split() = %q, want %q", got, want)
	}
}

func TestOversizedPhraseIsPreservedAndReported(t *testing.T) {
	segmenter, err := New(3)
	if err != nil {
		t.Fatal(err)
	}
	phrase := "один два три четыре пять шесть семь"
	result := segmenter.SplitDetailed(phrase)
	if !slices.Equal(result.Segments, []string{phrase}) {
		t.Fatalf("segments = %q", result.Segments)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
	warning := result.Warnings[0]
	if warning.Phrase != phrase || warning.Words != 7 || warning.Limit != 3 {
		t.Fatalf("warning = %#v", warning)
	}
}

func TestSceneBreakForcesBoundaryAndIsRemoved(t *testing.T) {
	segmenter, err := New(100)
	if err != nil {
		t.Fatal(err)
	}
	got := segmenter.Split("До разделителя.\n *   *\t* \nПосле разделителя.")
	want := []string{"До разделителя.", "После разделителя."}
	if !slices.Equal(got, want) {
		t.Fatalf("Split() = %q, want %q", got, want)
	}
}

func TestDecimalPointDoesNotSplitSentence(t *testing.T) {
	units := splitIntoUnits("Цена 3.14 рубля. «Правда?!» Да.")
	got := make([]string, 0, len(units))
	for _, unit := range units {
		if !unit.forceBoundary {
			got = append(got, unit.text)
		}
	}
	want := []string{"Цена 3.14 рубля.", "«Правда?!»", "Да."}
	if !slices.Equal(got, want) {
		t.Fatalf("units = %q, want %q", got, want)
	}
}

func TestZeroValueUsesDefaultLimit(t *testing.T) {
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
