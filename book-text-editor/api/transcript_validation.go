package api

import (
	"math"
	"strconv"
	"strings"
	"unicode"

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

	tokenBudget := transcriptTokenBudget(len(expectedTokens))
	if absInt(len(expectedTokens)-len(actualTokens)) > tokenBudget {
		return false
	}
	if boundedTokenEditDistance(expectedTokens, actualTokens, tokenBudget) > tokenBudget {
		return false
	}

	expectedRunes := []rune(strings.Join(expectedTokens, " "))
	actualRunes := []rune(strings.Join(actualTokens, " "))
	runeBudget := transcriptRuneBudget(max(len(expectedRunes), len(actualRunes)))
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
