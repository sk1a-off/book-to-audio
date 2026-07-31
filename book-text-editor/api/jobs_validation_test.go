package api

import "testing"

func TestTranscriptMatchesToleratesTypicalSTTNoise(t *testing.T) {
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
			name:     "number normalization",
			expected: "В 2026 году вышло 25 новых изданий этой большой серии.",
			actual:   "В две тысячи двадцать шестом году вышло двадцать пять новых изданий этой большой серии",
			want:     true,
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
			name:     "empty transcript",
			expected: "Непустой текст.",
			actual:   "",
			want:     false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := transcriptMatches(test.expected, test.actual); got != test.want {
				t.Fatalf("transcriptMatches() = %t, want %t", got, test.want)
			}
		})
	}
}
