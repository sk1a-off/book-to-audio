package api

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	combiningAcuteAccent       = '\u0301'
	maxPronunciationRules      = 256
	maxPronunciationRuleRunes  = 320
	maxPronunciationRulesBytes = 64 << 10
)

type pronunciationRule struct {
	sourceRunes []rune
	stressAfter []bool
}

func parsePronunciationRules(raw string) ([]pronunciationRule, error) {
	if len(raw) > maxPronunciationRulesBytes {
		return nil, fmt.Errorf("rules exceed %d bytes", maxPronunciationRulesBytes)
	}

	rules := make([]pronunciationRule, 0)
	seen := make(map[string]int)
	for index, line := range strings.Split(raw, "\n") {
		lineNumber := index + 1
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "=>")
		if len(parts) != 2 {
			return nil, fmt.Errorf("line %d must contain exactly one =>", lineNumber)
		}
		source := strings.TrimSpace(parts[0])
		stressed := strings.TrimSpace(parts[1])
		if source == "" || stressed == "" {
			return nil, fmt.Errorf("line %d has an empty side", lineNumber)
		}
		if utf8.RuneCountInString(source) > maxPronunciationRuleRunes {
			return nil, fmt.Errorf("line %d source exceeds %d characters", lineNumber, maxPronunciationRuleRunes)
		}
		if strings.ContainsRune(source, combiningAcuteAccent) {
			return nil, fmt.Errorf("line %d source must not contain U+0301", lineNumber)
		}

		stressAfter, stripped, accents, err := pronunciationStressMask(stressed)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		if accents == 0 {
			return nil, fmt.Errorf("line %d target must add at least one U+0301", lineNumber)
		}
		if !strings.EqualFold(source, stripped) {
			return nil, fmt.Errorf("line %d may only add U+0301 accents", lineNumber)
		}

		key := strings.ToLower(source)
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf("line %d duplicates source from line %d", lineNumber, previous)
		}
		seen[key] = lineNumber
		rules = append(rules, pronunciationRule{
			sourceRunes: []rune(source),
			stressAfter: stressAfter,
		})
		if len(rules) > maxPronunciationRules {
			return nil, fmt.Errorf("rules exceed %d entries", maxPronunciationRules)
		}
	}

	sort.SliceStable(rules, func(i, j int) bool {
		return len(rules[i].sourceRunes) > len(rules[j].sourceRunes)
	})
	return rules, nil
}

func pronunciationStressMask(stressed string) ([]bool, string, int, error) {
	stripped := make([]rune, 0, utf8.RuneCountInString(stressed))
	stressAfter := make([]bool, 0, cap(stripped))
	accents := 0
	for _, current := range stressed {
		if current != combiningAcuteAccent {
			stripped = append(stripped, current)
			stressAfter = append(stressAfter, false)
			continue
		}
		if len(stripped) == 0 || stressAfter[len(stressAfter)-1] {
			return nil, "", 0, fmt.Errorf("U+0301 must follow one letter")
		}
		if !isRussianVowel(stripped[len(stripped)-1]) {
			return nil, "", 0, fmt.Errorf("U+0301 must follow a Russian vowel")
		}
		stressAfter[len(stressAfter)-1] = true
		accents++
	}
	return stressAfter, string(stripped), accents, nil
}

func applyPronunciationSettings(text string, settings PronunciationGenerationSettings) (string, int, error) {
	if !settings.Enabled || strings.TrimSpace(settings.Rules) == "" {
		return text, 0, nil
	}
	rules, err := parsePronunciationRules(settings.Rules)
	if err != nil {
		return "", 0, err
	}

	source := []rune(text)
	var result strings.Builder
	result.Grow(len(text) + len(rules)*2)
	applied := 0
	for offset := 0; offset < len(source); {
		matched := false
		for _, rule := range rules {
			end := offset + len(rule.sourceRunes)
			if end > len(source) || !pronunciationBoundary(source, offset, end) ||
				!pronunciationRunesEqualFold(source[offset:end], rule.sourceRunes) {
				continue
			}
			for index, current := range source[offset:end] {
				result.WriteRune(current)
				if rule.stressAfter[index] {
					result.WriteRune(combiningAcuteAccent)
				}
			}
			offset = end
			applied++
			matched = true
			break
		}
		if matched {
			continue
		}
		result.WriteRune(source[offset])
		offset++
	}
	return result.String(), applied, nil
}

func pronunciationRunesEqualFold(left, right []rune) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if unicode.ToLower(left[index]) != unicode.ToLower(right[index]) {
			return false
		}
	}
	return true
}

func pronunciationBoundary(source []rune, start, end int) bool {
	if start < 0 || end > len(source) || start >= end {
		return false
	}
	if isPronunciationWordRune(source[start]) && start > 0 &&
		isPronunciationWordRune(source[start-1]) {
		return false
	}
	if isPronunciationWordRune(source[end-1]) && end < len(source) &&
		isPronunciationWordRune(source[end]) {
		return false
	}
	return true
}

func isPronunciationWordRune(current rune) bool {
	return unicode.IsLetter(current) || unicode.IsNumber(current) || current == '_' || current == '-'
}

func isRussianVowel(current rune) bool {
	switch unicode.ToLower(current) {
	case 'а', 'е', 'ё', 'и', 'о', 'у', 'ы', 'э', 'ю', 'я':
		return true
	default:
		return false
	}
}
