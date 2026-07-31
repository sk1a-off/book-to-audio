package api

import (
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	maxTranscriptTokenDistance = 12
	maxTranscriptRuneDistance  = 512
)

// transcriptMatches accepts harmless STT differences while keeping changes in
// wording, order and numeric values visible to the reviewer. Both edit-distance
// calculations are banded, so a malformed 20k-character fragment cannot cause
// quadratic work in the API process.
func transcriptMatches(expected, actual string) bool {
	expectedNormalized := normalizeValidationText(expected)
	actualNormalized := normalizeValidationText(actual)
	if expectedNormalized == actualNormalized {
		return expectedNormalized != ""
	}
	if expectedNormalized == "" || actualNormalized == "" {
		return false
	}

	expectedTokens := canonicalValidationTokens(expectedNormalized)
	actualTokens := canonicalValidationTokens(actualNormalized)
	if len(expectedTokens) <= 2 || len(actualTokens) <= 2 {
		return false
	}

	if !equalValidationTokenSequences(
		criticalValidationTokens(expectedTokens),
		criticalValidationTokens(actualTokens),
	) {
		return false
	}

	tokenBudget := transcriptTokenBudget(len(expectedTokens))
	if absInt(len(expectedTokens)-len(actualTokens)) > tokenBudget {
		return false
	}
	tokenStats := boundedTokenEditStats(
		expectedTokens,
		actualTokens,
		tokenBudget,
	)
	if tokenStats.Distance > tokenBudget ||
		(tokenStats.Insertions > 0 && tokenStats.Deletions > 0) {
		return false
	}

	expectedRunes := []rune(strings.Join(expectedTokens, " "))
	actualRunes := []rune(strings.Join(actualTokens, " "))
	runeBudget := min(
		maxTranscriptRuneDistance,
		transcriptRuneBudget(max(len(expectedRunes), len(actualRunes)))+
			tokenStats.IndelRunes,
	)
	return boundedRuneEditDistance(expectedRunes, actualRunes, runeBudget) <= runeBudget
}

func transcriptTokenBudget(tokenCount int) int {
	budget := int(math.Ceil(float64(tokenCount) * 0.08))
	if budget < 1 {
		budget = 1
	}
	if budget > maxTranscriptTokenDistance {
		budget = maxTranscriptTokenDistance
	}
	return budget
}

func transcriptRuneBudget(runeCount int) int {
	if runeCount <= 0 {
		return 0
	}
	ratio := 0.07
	if runeCount >= 160 {
		ratio = 0.10
	}
	budget := int(math.Ceil(float64(runeCount) * ratio))
	if budget < 1 {
		budget = 1
	}
	if budget > maxTranscriptRuneDistance {
		budget = maxTranscriptRuneDistance
	}
	return budget
}

func normalizeValidationText(value string) string {
	value = cases.Fold().String(norm.NFKD.String(value))
	var builder strings.Builder
	builder.Grow(len(value))
	previousSpace := true

	for _, character := range value {
		if unicode.Is(unicode.Mn, character) {
			continue
		}
		if character == 'ё' {
			character = 'е'
		}
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			builder.WriteRune(character)
			previousSpace = false
			continue
		}
		if !previousSpace {
			builder.WriteByte(' ')
			previousSpace = true
		}
	}
	return strings.TrimSpace(builder.String())
}

func canonicalValidationTokens(normalized string) []string {
	raw := strings.Fields(normalized)
	result := make([]string, 0, len(raw))
	for index := 0; index < len(raw); {
		if numeric, ok := canonicalDigitToken(raw[index]); ok {
			result = append(result, numeric)
			index++
			continue
		}
		value, consumed, ok := parseRussianNumber(raw[index:])
		if ok {
			result = append(result, "#"+strconv.FormatInt(value, 10))
			index += consumed
			continue
		}
		result = append(result, raw[index])
		index++
	}
	return result
}

func canonicalDigitToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	for _, character := range token {
		if !unicode.IsDigit(character) {
			return "", false
		}
	}
	trimmed := strings.TrimLeft(token, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	return "#" + trimmed, true
}

var russianNumberValues = map[string]int64{
	"ноль": 0, "нуль": 0,
	"один": 1, "одна": 1, "одно": 1, "первый": 1, "первая": 1, "первое": 1, "первого": 1,
	"два": 2, "две": 2, "второй": 2, "вторая": 2, "второе": 2, "второго": 2,
	"три": 3, "третий": 3, "третья": 3, "третье": 3, "третьего": 3,
	"четыре": 4, "четвертый": 4, "четвертая": 4, "четвертое": 4, "четвертом": 4,
	"пять": 5, "пятый": 5, "пятая": 5, "пятое": 5, "пятом": 5,
	"шесть": 6, "шестой": 6, "шестая": 6, "шестое": 6, "шестом": 6,
	"семь": 7, "седьмой": 7, "седьмая": 7, "седьмое": 7, "седьмом": 7,
	"восемь": 8, "восьмой": 8, "восьмая": 8, "восьмое": 8, "восьмом": 8,
	"девять": 9, "девятый": 9, "девятая": 9, "девятое": 9, "девятом": 9,
	"десять": 10, "одиннадцать": 11, "двенадцать": 12, "тринадцать": 13,
	"четырнадцать": 14, "пятнадцать": 15, "шестнадцать": 16,
	"семнадцать": 17, "восемнадцать": 18, "девятнадцать": 19,
	"двадцать": 20, "тридцать": 30, "сорок": 40, "пятьдесят": 50,
	"шестьдесят": 60, "семьдесят": 70, "восемьдесят": 80, "девяносто": 90,
	"сто": 100, "двести": 200, "триста": 300, "четыреста": 400,
	"пятьсот": 500, "шестьсот": 600, "семьсот": 700, "восемьсот": 800, "девятьсот": 900,
}

var russianNumberScales = map[string]int64{
	"тысяча": 1_000, "тысячи": 1_000, "тысяч": 1_000,
	"миллион": 1_000_000, "миллиона": 1_000_000, "миллионов": 1_000_000,
	"миллиард": 1_000_000_000, "миллиарда": 1_000_000_000, "миллиардов": 1_000_000_000,
}

func parseRussianNumber(tokens []string) (value int64, consumed int, ok bool) {
	var total, current int64
	for consumed < len(tokens) {
		token := tokens[consumed]
		if part, exists := russianNumberValues[token]; exists {
			current += part
			consumed++
			ok = true
			continue
		}
		if scale, exists := russianNumberScales[token]; exists {
			if current == 0 {
				current = 1
			}
			if current > math.MaxInt64/scale || total > math.MaxInt64-current*scale {
				return 0, 0, false
			}
			total += current * scale
			current = 0
			consumed++
			ok = true
			continue
		}
		break
	}
	if !ok {
		return 0, 0, false
	}
	if total > math.MaxInt64-current {
		return 0, 0, false
	}
	return total + current, consumed, true
}

func criticalValidationTokens(tokens []string) []string {
	result := make([]string, 0)
	for _, token := range tokens {
		if strings.HasPrefix(token, "#") {
			result = append(result, token)
			continue
		}
		switch token {
		case "не", "ни", "нет", "без":
			result = append(result, token)
		}
	}
	return result
}

func equalValidationTokenSequences(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type tokenEditStats struct {
	Distance   int
	Insertions int
	Deletions  int
	IndelRunes int
}

func boundedTokenEditStats(
	left, right []string,
	limit int,
) tokenEditStats {
	invalid := tokenEditStats{Distance: limit + 1}
	if limit < 0 || absInt(len(left)-len(right)) > limit {
		return invalid
	}

	previous := make([]tokenEditStats, len(right)+1)
	current := make([]tokenEditStats, len(right)+1)
	for index := range previous {
		previous[index] = invalid
		current[index] = invalid
	}
	previous[0] = tokenEditStats{}
	for rightIndex := 1; rightIndex <= min(len(right), limit); rightIndex++ {
		previous[rightIndex] = advanceTokenEditStats(
			previous[rightIndex-1],
			1,
			1,
			0,
			tokenRuneCost(right[rightIndex-1]),
			limit,
		)
	}

	for leftIndex := 1; leftIndex <= len(left); leftIndex++ {
		for index := range current {
			current[index] = invalid
		}
		if leftIndex <= limit {
			current[0] = advanceTokenEditStats(
				previous[0],
				1,
				0,
				1,
				tokenRuneCost(left[leftIndex-1]),
				limit,
			)
		}

		start := max(1, leftIndex-limit)
		end := min(len(right), leftIndex+limit)
		rowMinimum := limit + 1
		for rightIndex := start; rightIndex <= end; rightIndex++ {
			best := invalid
			best = betterTokenEditStats(
				best,
				advanceTokenEditStats(
					previous[rightIndex],
					1,
					0,
					1,
					tokenRuneCost(left[leftIndex-1]),
					limit,
				),
			)
			best = betterTokenEditStats(
				best,
				advanceTokenEditStats(
					current[rightIndex-1],
					1,
					1,
					0,
					tokenRuneCost(right[rightIndex-1]),
					limit,
				),
			)

			leftToken := left[leftIndex-1]
			rightToken := right[rightIndex-1]
			switch {
			case leftToken == rightToken:
				best = betterTokenEditStats(best, previous[rightIndex-1])
			case validationTokensSimilar(leftToken, rightToken):
				best = betterTokenEditStats(
					best,
					advanceTokenEditStats(
						previous[rightIndex-1],
						1,
						0,
						0,
						0,
						limit,
					),
				)
			}
			current[rightIndex] = best
			rowMinimum = min(rowMinimum, best.Distance)
		}
		if rowMinimum > limit {
			return invalid
		}
		previous, current = current, previous
	}

	if previous[len(right)].Distance > limit {
		return invalid
	}
	return previous[len(right)]
}

func advanceTokenEditStats(
	stats tokenEditStats,
	distance, insertions, deletions, indelRunes, limit int,
) tokenEditStats {
	if stats.Distance > limit {
		return tokenEditStats{Distance: limit + 1}
	}
	stats.Distance += distance
	stats.Insertions += insertions
	stats.Deletions += deletions
	stats.IndelRunes += indelRunes
	if stats.Distance > limit {
		return tokenEditStats{Distance: limit + 1}
	}
	return stats
}

func betterTokenEditStats(current, candidate tokenEditStats) tokenEditStats {
	if candidate.Distance != current.Distance {
		if candidate.Distance < current.Distance {
			return candidate
		}
		return current
	}
	candidateIndels := candidate.Insertions + candidate.Deletions
	currentIndels := current.Insertions + current.Deletions
	if candidateIndels != currentIndels {
		if candidateIndels < currentIndels {
			return candidate
		}
		return current
	}
	if candidate.IndelRunes < current.IndelRunes {
		return candidate
	}
	return current
}

func validationTokensSimilar(left, right string) bool {
	if strings.HasPrefix(left, "#") || strings.HasPrefix(right, "#") {
		return false
	}
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	longest := max(len(leftRunes), len(rightRunes))
	if longest < 5 {
		return false
	}
	budget := max(1, longest/5)
	return boundedRuneEditDistance(leftRunes, rightRunes, budget) <= budget
}

func tokenRuneCost(token string) int {
	return utf8.RuneCountInString(token) + 1
}

func boundedTokenEditDistance(left, right []string, limit int) int {
	return boundedEditDistance(
		len(left),
		len(right),
		limit,
		func(i, j int) bool { return left[i] == right[j] },
	)
}

func boundedRuneEditDistance(left, right []rune, limit int) int {
	return boundedEditDistance(
		len(left),
		len(right),
		limit,
		func(i, j int) bool { return left[i] == right[j] },
	)
}

func boundedEditDistance(
	leftLength, rightLength, limit int,
	equal func(int, int) bool,
) int {
	if limit < 0 || absInt(leftLength-rightLength) > limit {
		return limit + 1
	}
	if leftLength == 0 {
		return rightLength
	}
	if rightLength == 0 {
		return leftLength
	}

	infinity := limit + 1
	previous := make([]int, rightLength+1)
	current := make([]int, rightLength+1)
	for index := range previous {
		if index <= limit {
			previous[index] = index
		} else {
			previous[index] = infinity
		}
	}

	for leftIndex := 1; leftIndex <= leftLength; leftIndex++ {
		for index := range current {
			current[index] = infinity
		}
		if leftIndex <= limit {
			current[0] = leftIndex
		}
		start := max(1, leftIndex-limit)
		end := min(rightLength, leftIndex+limit)
		rowMinimum := infinity
		for rightIndex := start; rightIndex <= end; rightIndex++ {
			cost := 1
			if equal(leftIndex-1, rightIndex-1) {
				cost = 0
			}
			current[rightIndex] = min(
				previous[rightIndex]+1,
				current[rightIndex-1]+1,
				previous[rightIndex-1]+cost,
			)
			rowMinimum = min(rowMinimum, current[rightIndex])
		}
		if rowMinimum > limit {
			return infinity
		}
		previous, current = current, previous
	}
	if previous[rightLength] > limit {
		return infinity
	}
	return previous[rightLength]
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
