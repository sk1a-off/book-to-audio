package api

import (
	"strings"
	"testing"
)

func TestTranscriptMatchesIsDeliberatelyForgiving(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		expected string
		actual   string
		want     bool
	}{
		{
			name:     "punctuation case and yo",
			expected: "Ёлка, гори! Это было в 2026 году.",
			actual:   "елка гори это было в 2026 году",
			want:     true,
		},
		{
			name:     "several omissions remain acceptable",
			expected: "Однажды вечером путник очень медленно вошёл в старый пустой дом возле реки.",
			actual:   "Однажды путник вошел в старый дом возле реки",
			want:     true,
		},
		{
			name:     "word order and numeric difference do not flood warnings",
			expected: "В серии было пять книг и одна рукопись.",
			actual:   "Одна рукопись и пятьсот книг были в серии",
			want:     true,
		},
		{
			name:     "short inflection is acceptable",
			expected: "Для нас обоих",
			actual:   "Для них обоих",
			want:     true,
		},
		{
			name:     "unrelated text is warning",
			expected: "Однажды вечером путник медленно вошёл в старый пустой дом.",
			actual:   "Утром поезд быстро покинул новый большой город.",
			want:     false,
		},
		{
			name:     "almost entirely missing is warning",
			expected: "Это длинный фрагмент книги, который должен быть распознан хотя бы в основных словах.",
			actual:   "фрагмент",
			want:     false,
		},
		{
			name:     "empty transcript is warning",
			expected: "Непустой текст.",
			actual:   "",
			want:     false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := transcriptMatches(test.expected, test.actual); got != test.want {
				t.Fatalf("transcriptMatches() = %t, want %t; score=%.3f",
					got, test.want, transcriptSimilarityScore(test.expected, test.actual))
			}
		})
	}
}

func TestTranscriptSimilarityRanksBetterAttemptHigher(t *testing.T) {
	t.Parallel()
	expected := "Путник медленно вошёл в старый дом и закрыл тяжёлую дверь."
	poor := transcriptSimilarityScore(expected, "Поезд покинул город утром")
	better := transcriptSimilarityScore(expected, "Путник вошел в старый дом и закрыл дверь")
	if better <= poor {
		t.Fatalf("better score %.3f <= poor score %.3f", better, poor)
	}
	if better < transcriptWarningSimilarityThreshold {
		t.Fatalf("better score %.3f unexpectedly creates warning", better)
	}
}

func TestTranscriptSimilarityLongInputIsLinearAndBounded(t *testing.T) {
	t.Parallel()
	expected := strings.Repeat("длинный проверяемый фрагмент ", 700)
	actual := strings.Repeat("совершенно другой материал ", 700)
	if transcriptMatches(expected, actual) {
		t.Fatal("unrelated long input was accepted")
	}
}

func TestCanonicalValidationTokensStillNormalizeNumbers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ input, want string }{
		{input: "двадцать пять", want: "#25"},
		{input: "0025", want: "#25"},
		{input: "две тысячи двадцать шестом", want: "#2026"},
		{input: "пятьсот", want: "#500"},
	} {
		got := strings.Join(canonicalValidationTokens(test.input), " ")
		if got != test.want {
			t.Errorf("canonicalValidationTokens(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}
