// Package segment splits book text into TTS-friendly segments.
package segment

import (
	"fmt"
	"strings"
	"unicode"
)

// DefaultMaxWords is the preferred upper bound for a single segment.
const DefaultMaxWords = 60

const sceneBreak = "* * *"

// Warning describes a source phrase that cannot fit into the preferred limit
// without destroying its sentence boundary.
type Warning struct {
	Phrase string
	Words  int
	Limit  int
}

func (w Warning) Error() string {
	return fmt.Sprintf(
		"phrase contains %d words and cannot fit the %d-word limit",
		w.Words,
		w.Limit,
	)
}

// Result contains ordered segments and non-fatal segmentation diagnostics.
type Result struct {
	Segments []string
	Warnings []Warning
}

// Segmenter splits text at semantic boundaries. The zero value is ready to use.
type Segmenter struct {
	maxWords int
}

// New creates a Segmenter with the requested preferred word limit.
func New(maxWords int) (Segmenter, error) {
	if maxWords <= 0 {
		return Segmenter{}, fmt.Errorf("max words must be positive, got %d", maxWords)
	}
	return Segmenter{maxWords: maxWords}, nil
}

// MaxWords returns the effective preferred word limit.
func (s Segmenter) MaxWords() int {
	if s.maxWords == 0 {
		return DefaultMaxWords
	}
	return s.maxWords
}

// Split is the compatibility entry point used by the FB2 parser.
func (s Segmenter) Split(text string) []string {
	return s.SplitDetailed(text).Segments
}

// SplitDetailed packs complete phrases up to MaxWords. A single phrase longer
// than the limit is kept intact and reported as a warning instead of being cut
// at an arbitrary word, which produces unnatural TTS and false STT warnings.
func (s Segmenter) SplitDetailed(text string) Result {
	units := splitIntoUnits(text)
	if len(units) == 0 {
		return Result{}
	}

	limit := s.MaxWords()
	result := Result{
		Segments: make([]string, 0, len(units)),
		Warnings: make([]Warning, 0),
	}
	currentPhrases := make([]string, 0)
	currentWords := 0

	flush := func() {
		if len(currentPhrases) == 0 {
			return
		}
		result.Segments = append(result.Segments, strings.Join(currentPhrases, " "))
		currentPhrases = currentPhrases[:0]
		currentWords = 0
	}

	for _, unit := range units {
		if unit.forceBoundary {
			flush()
			continue
		}

		wordCount := countWords(unit.text)
		if wordCount > limit {
			flush()
			result.Segments = append(result.Segments, unit.text)
			result.Warnings = append(result.Warnings, Warning{
				Phrase: unit.text,
				Words:  wordCount,
				Limit:  limit,
			})
			continue
		}
		if currentWords > 0 && currentWords+wordCount > limit {
			flush()
		}
		currentPhrases = append(currentPhrases, unit.text)
		currentWords += wordCount
	}
	flush()
	return result
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
	for _, line := range strings.Split(text, "\n") {
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
	return runes[index] == '.' && index > 0 && index+1 < len(runes) &&
		unicode.IsDigit(runes[index-1]) && unicode.IsDigit(runes[index+1])
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
