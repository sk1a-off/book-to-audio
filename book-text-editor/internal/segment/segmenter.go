// Package segment splits book text into TTS-friendly segments.
package segment

import (
	"fmt"
	"strings"
	"unicode"
)

// DefaultMaxWords is the default upper bound for a single segment.
const DefaultMaxWords = 60

const sceneBreak = "* * *"

// Segmenter splits text while guaranteeing that no segment exceeds MaxWords.
type Segmenter struct {
	maxWords int
}

// New creates a Segmenter with the requested word limit.
func New(maxWords int) (Segmenter, error) {
	if maxWords <= 0 {
		return Segmenter{}, fmt.Errorf("max words must be positive, got %d", maxWords)
	}

	return Segmenter{maxWords: maxWords}, nil
}

// MaxWords returns the effective word limit.
//
// The zero value is ready to use and applies DefaultMaxWords.
func (s Segmenter) MaxWords() int {
	if s.maxWords == 0 {
		return DefaultMaxWords
	}

	return s.maxWords
}

// Split divides text at sentence boundaries and packs phrases up to MaxWords.
// A phrase longer than the limit is split by words as a safe fallback.
// The scene-break marker "* * *" is removed and forces a segment boundary.
func (s Segmenter) Split(text string) []string {
	units := splitIntoUnits(text)
	if len(units) == 0 {
		return nil
	}

	limit := s.MaxWords()
	segments := make([]string, 0, len(units))
	currentPhrases := make([]string, 0)
	currentWords := 0

	flush := func() {
		if len(currentPhrases) == 0 {
			return
		}

		segments = append(segments, strings.Join(currentPhrases, " "))
		currentPhrases = currentPhrases[:0]
		currentWords = 0
	}

	for _, unit := range units {
		if unit.forceBoundary {
			flush()
			continue
		}

		for _, part := range splitByWordLimit(unit.text, limit) {
			wordCount := countWords(part)
			if currentWords > 0 && currentWords+wordCount > limit {
				flush()
			}

			currentPhrases = append(currentPhrases, part)
			currentWords += wordCount
		}
	}

	flush()

	return segments
}

func splitByWordLimit(text string, limit int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if len(words) <= limit {
		return []string{strings.Join(words, " ")}
	}

	parts := make([]string, 0, (len(words)+limit-1)/limit)
	for start := 0; start < len(words); start += limit {
		end := min(start+limit, len(words))
		parts = append(parts, strings.Join(words[start:end], " "))
	}

	return parts
}

func countWords(text string) int {
	return len(strings.Fields(text))
}

type textUnit struct {
	text          string
	forceBoundary bool
}

func splitIntoUnits(text string) []textUnit {
	var units []textUnit

	for line := range strings.Lines(text) {
		line = normalizeSpace(line)
		if line == "" {
			continue
		}

		for {
			before, after, found := strings.Cut(line, sceneBreak)
			units = appendPhrases(units, before)
			if !found {
				break
			}

			units = append(units, textUnit{forceBoundary: true})
			line = after
		}
	}

	return units
}

func appendPhrases(units []textUnit, text string) []textUnit {
	text = strings.TrimSpace(text)
	if text == "" {
		return units
	}

	for _, phrase := range splitLine(text) {
		units = append(units, textUnit{text: phrase})
	}

	return units
}

func splitLine(line string) []string {
	runes := []rune(line)
	var phrases []string
	start := 0

	for index := 0; index < len(runes); index++ {
		if !isSentenceEnd(runes[index]) || isDecimalPoint(runes, index) {
			continue
		}

		end := index + 1
		for end < len(runes) && isSentenceEnd(runes[end]) {
			end++
		}
		for end < len(runes) && isClosingRune(runes[end]) {
			end++
		}

		if end < len(runes) && !unicode.IsSpace(runes[end]) {
			continue
		}

		if phrase := strings.TrimSpace(string(runes[start:end])); phrase != "" {
			phrases = append(phrases, phrase)
		}

		for end < len(runes) && unicode.IsSpace(runes[end]) {
			end++
		}

		start = end
		index = end - 1
	}

	if tail := strings.TrimSpace(string(runes[start:])); tail != "" {
		phrases = append(phrases, tail)
	}

	return phrases
}

func isDecimalPoint(runes []rune, index int) bool {
	return runes[index] == '.' &&
		index > 0 &&
		index+1 < len(runes) &&
		unicode.IsDigit(runes[index-1]) &&
		unicode.IsDigit(runes[index+1])
}

func normalizeSpace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', '…', '。', '！', '？':
		return true
	default:
		return false
	}
}

func isClosingRune(r rune) bool {
	switch r {
	case '"', '\'', '»', '”', '’', ')', ']', '}':
		return true
	default:
		return false
	}
}
