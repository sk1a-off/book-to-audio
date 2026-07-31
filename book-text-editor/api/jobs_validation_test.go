package api

import (
	"math"
	"strings"
	"testing"
)

func TestTranscriptMatchesUsesNinetyTwoPercentThreshold(t *testing.T) {
	t.Parallel()

	if math.Abs(transcriptWarningSimilarityThreshold-0.92) > 1e-12 {
		t.Fatalf(
			"transcriptWarningSimilarityThreshold = %.12f, want 0.92",
			transcriptWarningSimilarityThreshold,
		)
	}

	tests := []struct {
		name       string
		expected   string
		actual     string
		wantMatch  bool
		scoreAbove bool
	}{
		{
			name:       "punctuation case and yo are identical after normalization",
			expected:   "Ёлка, гори! Это было в 2026 году.",
			actual:     "елка гори это было в 2026 году",
			wantMatch:  true,
			scoreAbove: true,
		},
		{
			name:       "one spelling error in a long phrase remains above threshold",
			expected:   "Однажды вечером путник очень медленно вошёл в старый каменный дом возле тихой реки и осторожно закрыл тяжёлую дверь.",
			actual:     "Однажды вечером путник очень медленно вошел в старый каменый дом возле тихой реки и осторожно закрыл тяжелую дверь.",
			wantMatch:  true,
			scoreAbove: true,
		},
		{
			name:       "one material omission below threshold creates warning",
			expected:   "Путник медленно вошёл в старый каменный дом.",
			actual:     "Путник медленно вошел в старый дом",
			wantMatch:  false,
			scoreAbove: false,
		},
		{
			name:       "several omissions create warning",
			expected:   "Однажды вечером путник очень медленно вошёл в старый пустой дом возле реки.",
			actual:     "Однажды путник вошел в старый дом возле реки",
			wantMatch:  false,
			scoreAbove: false,
		},
		{
			name:       "word order and numeric difference create warning below 92 percent",
			expected:   "В серии было пять книг и одна рукопись.",
			actual:     "Одна рукопись и пятьсот книг были в серии",
			wantMatch:  false,
			scoreAbove: false,
		},
		{
			name:       "unrelated text creates warning",
			expected:   "Однажды вечером путник медленно вошёл в старый пустой дом.",
			actual:     "Утром поезд быстро покинул новый большой город.",
			wantMatch:  false,
			scoreAbove: false,
		},
		{
			name:       "empty transcript creates warning",
			expected:   "Непустой текст.",
			actual:     "",
			wantMatch:  false,
			scoreAbove: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			score := transcriptSimilarityScore(test.expected, test.actual)
			if got := transcriptMatches(test.expected, test.actual); got != test.wantMatch {
				t.Fatalf(
					"transcriptMatches() = %t, want %t; score=%.6f threshold=%.2f",
					got,
					test.wantMatch,
					score,
					transcriptWarningSimilarityThreshold,
				)
			}
			if test.scoreAbove && score < transcriptWarningSimilarityThreshold {
				t.Fatalf("score %.6f is below threshold %.2f", score, transcriptWarningSimilarityThreshold)
			}
			if !test.scoreAbove && score >= transcriptWarningSimilarityThreshold {
				t.Fatalf("score %.6f is not below threshold %.2f", score, transcriptWarningSimilarityThreshold)
			}
		})
	}
}

func TestTranscriptSimilarityRanksBestAttemptHigher(t *testing.T) {
	t.Parallel()

	expected := "Однажды вечером путник очень медленно вошёл в старый каменный дом возле тихой реки и осторожно закрыл тяжёлую дверь."
	poor := transcriptSimilarityScore(expected, "Поезд покинул город утром")
	acceptable := transcriptSimilarityScore(
		expected,
		"Однажды вечером путник очень медленно вошел в старый каменый дом возле тихой реки и осторожно закрыл тяжелую дверь.",
	)
	perfect := transcriptSimilarityScore(expected, expected)

	if !(perfect > acceptable && acceptable > poor) {
		t.Fatalf(
			"scores are not ordered: perfect=%.3f acceptable=%.3f poor=%.3f",
			perfect,
			acceptable,
			poor,
		)
	}
	if acceptable < transcriptWarningSimilarityThreshold {
		t.Fatalf(
			"acceptable score %.3f unexpectedly creates warning at %.2f",
			acceptable,
			transcriptWarningSimilarityThreshold,
		)
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
