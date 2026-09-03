package segment

import (
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestNewRejectsInvalidLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{-1, 0} {
		if _, err := New(limit); err == nil {
			t.Fatalf("New(%d) returned nil error", limit)
		}
	}
}

func TestNewWithProfileRejectsUnknownProfile(t *testing.T) {
	t.Parallel()
	if _, err := NewWithProfile(Profile("future"), 60); err == nil {
		t.Fatal("NewWithProfile(future) error = nil")
	}
}

func TestProductionDefaultRemainsLegacyV1(t *testing.T) {
	t.Parallel()
	segmenter, err := New(60)
	if err != nil {
		t.Fatal(err)
	}
	if segmenter.Profile() != LegacyV1 {
		t.Fatalf("Profile() = %q, want %q", segmenter.Profile(), LegacyV1)
	}
	if got, want := segmenter.Split("Первая строка.\nВторая строка."),
		[]string{"Первая строка. Вторая строка."}; !slices.Equal(got, want) {
		t.Fatalf("legacy Split() = %q, want %q", got, want)
	}
}

func TestProsodyV2PreservesStructuralLineBoundaries(t *testing.T) {
	t.Parallel()
	segmenter, err := NewWithProfile(ProsodyV2, 60)
	if err != nil {
		t.Fatal(err)
	}
	text := "Глава 7\nПервый снег выпал ночью. К утру город стал неузнаваем."
	want := []string{
		"Глава 7",
		"Первый снег выпал ночью. К утру город стал неузнаваем.",
	}
	if got := segmenter.Split(text); !slices.Equal(got, want) {
		t.Fatalf("Split() = %q, want %q", got, want)
	}
}

func TestProsodyV2ProtectsInitialsAndRussianAbbreviations(t *testing.T) {
	t.Parallel()
	segmenter, err := NewWithProfile(ProsodyV2, 4)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		text string
		want []string
	}{
		{
			text: "А. С. Пушкин приехал. Потом уехал.",
			want: []string{"А. С. Пушкин приехал.", "Потом уехал."},
		},
		{
			text: "См. рис. 3 в книге. Потом продолжим.",
			want: []string{"См. рис. 3 в книге.", "Потом продолжим."},
		},
		{
			text: "Это важно, т. е. отступать нельзя. Потом продолжим.",
			want: []string{"Это важно, т. е. отступать нельзя.", "Потом продолжим."},
		},
		{
			text: "Он взял хлеб и т. д. Потом ушёл.",
			want: []string{"Он взял хлеб и т. д.", "Потом ушёл."},
		},
	}
	for _, test := range tests {
		if got := segmenter.Split(test.text); !slices.Equal(got, test.want) {
			t.Errorf("Split(%q) = %q, want %q", test.text, got, test.want)
		}
	}
}

func TestProsodyV2HardLimitUsesClauseThenDeterministicForcedSplit(t *testing.T) {
	t.Parallel()
	segmenter, err := NewWithProfile(ProsodyV2, 60)
	if err != nil {
		t.Fatal(err)
	}
	clause := strings.Repeat("слово ", 70) + ", " + strings.Repeat("дальше ", 70)
	result := segmenter.SplitDetailed(strings.TrimSpace(clause))
	if len(result.Segments) < 2 || len(result.Warnings) != 1 {
		t.Fatalf("clause result = %#v", result)
	}
	if result.Warnings[0].Kind != "hard_limit_clause_split" {
		t.Fatalf("clause warning = %#v", result.Warnings[0])
	}
	assertWithinOmniVoiceLimits(t, result.Segments)

	forced := strings.TrimSpace(strings.Repeat("слово ", OmniVoiceHardMaxWords+5))
	result = segmenter.SplitDetailed(forced)
	if len(result.Segments) != 2 || len(result.Warnings) != 1 ||
		result.Warnings[0].Kind != "hard_limit_forced_split" {
		t.Fatalf("forced result = %#v", result)
	}
	assertWithinOmniVoiceLimits(t, result.Segments)
}

func TestProsodyV2HardCharacterLimitDoesNotDropLongToken(t *testing.T) {
	t.Parallel()
	segmenter, err := NewWithProfile(ProsodyV2, 60)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("я", OmniVoiceHardMaxRunes+7)
	result := segmenter.SplitDetailed(source)
	if len(result.Segments) != 2 || strings.Join(result.Segments, "") != source {
		t.Fatalf("long token was not conserved: segment lengths=%v", runeLengths(result.Segments))
	}
	assertWithinOmniVoiceLimits(t, result.Segments)
}

func assertWithinOmniVoiceLimits(t *testing.T, segments []string) {
	t.Helper()
	for index, value := range segments {
		if words := countWords(value); words > OmniVoiceHardMaxWords {
			t.Errorf("segment[%d] words=%d", index, words)
		}
		if runes := utf8.RuneCountInString(value); runes > OmniVoiceHardMaxRunes {
			t.Errorf("segment[%d] runes=%d", index, runes)
		}
	}
}

func runeLengths(values []string) []int {
	result := make([]int, len(values))
	for index, value := range values {
		result[index] = utf8.RuneCountInString(value)
	}
	return result
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

func TestSplitReportsWarningsThroughHandler(t *testing.T) {
	t.Parallel()
	segmenter, err := New(2)
	if err != nil {
		t.Fatal(err)
	}
	var warnings []Warning
	segmenter = segmenter.WithWarningHandler(func(warning Warning) {
		warnings = append(warnings, warning)
	})
	segments := segmenter.Split("один два три четыре.")
	if got, want := segments, []string{"один два три четыре."}; !slices.Equal(got, want) {
		t.Fatalf("segments = %q, want %q", got, want)
	}
	if len(warnings) != 1 || warnings[0].Words != 4 || warnings[0].Limit != 2 {
		t.Fatalf("warnings = %#v", warnings)
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

func FuzzProsodyV2ConservesNonWhitespaceAndHardLimits(f *testing.F) {
	f.Add("Глава 7\nПервый снег выпал ночью. К утру город стал неузнаваем.")
	f.Add("А. С. Пушкин открыл книгу. См. рис. 3.")
	f.Add(strings.Repeat("длинное ", OmniVoiceHardMaxWords+5))

	f.Fuzz(func(t *testing.T, text string) {
		if strings.Contains(text, sceneBreak) {
			t.Skip()
		}
		segmenter, err := NewWithProfile(ProsodyV2, 60)
		if err != nil {
			t.Fatal(err)
		}
		result := segmenter.SplitDetailed(text)
		got := strings.Map(removeWhitespace, strings.Join(result.Segments, ""))
		want := strings.Map(removeWhitespace, text)
		if got != want {
			t.Fatalf("non-whitespace text changed: got=%q want=%q", got, want)
		}
		assertWithinOmniVoiceLimits(t, result.Segments)
	})
}

func removeWhitespace(character rune) rune {
	if unicode.IsSpace(character) || character == '\u200b' || character == '\ufeff' {
		return -1
	}
	return character
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
