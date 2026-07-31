package api

import (
	"strings"
	"testing"
)

func TestTranscriptMatches(t *testing.T) {
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
			name:     "one token omission in long phrase",
			expected: "Однажды вечером путник медленно вошёл в старый пустой дом.",
			actual:   "Однажды вечером путник вошел в старый пустой дом",
			want:     true,
		},
		{
			name:     "digits and spoken russian number",
			expected: "В 2026 году вышло 25 новых изданий этой большой серии.",
			actual:   "В две тысячи двадцать шестом году вышло двадцать пять новых изданий этой большой серии",
			want:     true,
		},
		{
			name:     "different numeric value",
			expected: "В серии было пять книг и одна рукопись.",
			actual:   "В серии было пятьсот книг и одна рукопись.",
			want:     false,
		},
		{
			name:     "single semantic token replacement",
			expected: "Однажды вечером путник медленно вошёл в старый пустой дом.",
			actual:   "Однажды вечером путник медленно вошёл в новый пустой дом.",
			want:     false,
		},
		{
			name:     "negation omission",
			expected: "Путник никогда не открывал эту старую тяжёлую дверь.",
			actual:   "Путник никогда открывал эту старую тяжёлую дверь.",
			want:     false,
		},
		{
			name:     "material replacement",
			expected: "Однажды вечером путник медленно вошёл в старый пустой дом.",
			actual:   "Утром поезд быстро покинул новый большой город.",
			want:     false,
		},
		{
			name:     "short phrase remains strict",
			expected: "Для нас обоих",
			actual:   "Для них обоих",
			want:     false,
		},
		{
			name:     "reordered phrase",
			expected: "Красный поезд медленно подошёл к дальней платформе.",
			actual:   "К дальней платформе медленно подошёл красный поезд.",
			want:     false,
		},
		{
			name:     "empty transcript",
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
				t.Fatalf("transcriptMatches() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestTranscriptMatchesLongInputIsBounded(t *testing.T) {
	t.Parallel()

	expected := strings.Repeat("длинный проверяемый фрагмент ", 700)
	actual := strings.Repeat("совершенно другой материал ", 700)
	if transcriptMatches(expected, actual) {
		t.Fatal("transcriptMatches() accepted a material long-input suffix")
	}
}

func TestCanonicalValidationTokensPreserveNumericMeaning(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		input string
		want  string
	}{
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
