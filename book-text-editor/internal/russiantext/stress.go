package russiantext

import (
	"sort"
	"strings"
	"unicode"
)

const combiningAcute = '\u0301'

// These phrase-scoped rules come from listener-accepted OmniVoice samples.
// Ambiguous or unseen contexts remain unchanged for manual review.
var selectiveStressRules = buildStressRules([]stressRuleSource{
	{plain: "замок на двери", stressed: "замо́к на двери"},
	{plain: "дверной замок", stressed: "дверной замо́к"},
	{plain: "замок на вершине", stressed: "за́мок на вершине"},
	{plain: "старинный замок стоял", stressed: "старинный за́мок стоял"},
	{plain: "вершине холма", stressed: "вершине холма́"},
	{plain: "белая мука", stressed: "белая мука́"},
	{plain: "невыносимая мука", stressed: "невыносимая му́ка"},
	{plain: "плачу за", stressed: "плачу́ за"},
	{plain: "плачу потому", stressed: "пла́чу потому"},
	{plain: "географический атлас", stressed: "географический а́тлас"},
	{plain: "из синего атласа", stressed: "из синего атла́са"},
	{plain: "государственный орган", stressed: "государственный о́рган"},
	{plain: "старинный орган", stressed: "старинный орга́н"},
	{plain: "коридор стал уже", stressed: "коридор стал у́же"},
	{plain: "мы уже не", stressed: "мы уже́ не"},
})

type stressRuleSource struct {
	plain    string
	stressed string
}

type stressRule struct {
	plain []rune
	mark  []bool
}

func buildStressRules(sources []stressRuleSource) []stressRule {
	rules := make([]stressRule, 0, len(sources))
	for _, source := range sources {
		plain := []rune(source.plain)
		stripped := make([]rune, 0, len(plain))
		marks := make([]bool, 0, len(plain))
		for _, current := range []rune(source.stressed) {
			if current == combiningAcute {
				if len(marks) == 0 || marks[len(marks)-1] {
					panic("invalid built-in selective stress rule")
				}
				marks[len(marks)-1] = true
				continue
			}
			stripped = append(stripped, current)
			marks = append(marks, false)
		}
		if !strings.EqualFold(string(plain), string(stripped)) {
			panic("built-in selective stress rule changes source letters")
		}
		rules = append(rules, stressRule{plain: plain, mark: marks})
	}
	sort.SliceStable(rules, func(left, right int) bool {
		return len(rules[left].plain) > len(rules[right].plain)
	})
	return rules
}

func applySelectiveStress(text string) (string, int) {
	source := []rune(text)
	var result strings.Builder
	result.Grow(len(text) + 16)
	applied := 0
	for offset := 0; offset < len(source); {
		matched := false
		for _, rule := range selectiveStressRules {
			end := offset + len(rule.plain)
			if end > len(source) || !phraseBoundary(source, offset, end) ||
				!runesEqualFold(source[offset:end], rule.plain) {
				continue
			}
			for index, current := range source[offset:end] {
				result.WriteRune(current)
				if rule.mark[index] {
					result.WriteRune(combiningAcute)
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
	return result.String(), applied
}

func runesEqualFold(left, right []rune) bool {
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

func phraseBoundary(source []rune, start, end int) bool {
	if start < 0 || start >= end || end > len(source) {
		return false
	}
	if start > 0 && isWordRune(source[start]) && isWordRune(source[start-1]) {
		return false
	}
	if end < len(source) && isWordRune(source[end-1]) && isWordRune(source[end]) {
		return false
	}
	return true
}

func isWordRune(current rune) bool {
	return unicode.IsLetter(current) || unicode.IsNumber(current) ||
		current == '_' || current == '-'
}
