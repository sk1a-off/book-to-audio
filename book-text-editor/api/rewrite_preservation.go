package api

import (
	"errors"
	"slices"
	"unicode"
)

var errRewriteChangedPhrase = errors.New(
	"rewriter changed protected phrase content",
)

// validateRewritePreservesPhrase is the final deterministic guard after the
// probabilistic text model. The model may improve punctuation, spacing,
// capitalization, е/ё and add a combining acute accent, but it may not add,
// remove, reorder, or replace letters, numbers, symbols, or inline controls.
//
// Number expansion is deliberately rejected here. OmniVoice's own text
// normalizer handles numbers without changing the source-of-truth fragment.
func validateRewritePreservesPhrase(original, rewritten string) error {
	if canonicalPhraseContent(original) != canonicalPhraseContent(rewritten) {
		return errRewriteChangedPhrase
	}
	if !slices.Equal(
		inlineControls(original),
		inlineControls(rewritten),
	) {
		return errRewriteChangedPhrase
	}
	return nil
}

func canonicalPhraseContent(value string) string {
	result := make([]rune, 0, len([]rune(value)))
	for _, current := range value {
		if current == '\u0301' {
			continue
		}
		if !unicode.IsLetter(current) &&
			!unicode.IsNumber(current) &&
			!unicode.IsSymbol(current) {
			continue
		}
		current = unicode.ToLower(current)
		if current == 'ё' {
			current = 'е'
		}
		result = append(result, current)
	}
	return string(result)
}

func inlineControls(value string) []string {
	runes := []rune(value)
	result := make([]string, 0)
	for index := 0; index < len(runes); index++ {
		closing, ok := controlDelimiter(runes[index])
		if !ok {
			continue
		}
		for end := index + 1; end < len(runes); end++ {
			if runes[end] != closing {
				continue
			}
			result = append(result, string(runes[index:end+1]))
			index = end
			break
		}
	}
	return result
}

func controlDelimiter(opening rune) (rune, bool) {
	switch opening {
	case '[':
		return ']', true
	case '{':
		return '}', true
	case '<':
		return '>', true
	default:
		return 0, false
	}
}
