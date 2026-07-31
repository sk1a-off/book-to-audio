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
	// A mismatch is actionable only when less than roughly half of the useful
	// lexical/character signal survives normalization. This deliberately avoids
	// flooding a long audiobook with warnings for punctuation, word order,
	// endings, numbers or a few missed words.
	transcriptWarningSimilarityThreshold = 0.48
	maxTranscriptTokenDistance           = 256
	maxTranscriptRuneDistance            = 2048
)

// transcriptMatches is intentionally forgiving. Whisper is used as a coarse
// quality signal, not as an exact proof-reader. Review is requested only when
// the recognized text is substantially different from the source.
func transcriptMatches(expected, actual string) bool {
	return transcriptSimilarityScore(expected, actual) >=
		transcriptWarningSimilarityThreshold
}

// transcriptSimilarityScore returns a stable score in [0, 1]. It combines a
// multiset token Dice score, character-bigram Dice score and length ratio. The
// algorithm is linear in input size and remains safe for the 20k-rune fragment
// limit. Word order and isolated numeric differences have intentionally small
// influence, while empty or unrelated transcripts score close to zero.
func transcriptSimilarityScore(expected, actual string) float64 {
	expectedNormalized := normalizeValidationText(expected)
	actualNormalized := normalizeValidationText(actual)
	if expectedNormalized == "" || actualNormalized == "" {
		return 0
	}
	if expectedNormalized == actualNormalized {
		return 1
	}

	expectedTokens := canonicalValidationTokens(expectedNormalized)
	actualTokens := canonicalValidationTokens(actualNormalized)
	tokenScore := validationTokenDice(expectedTokens, actualTokens)
	characterScore := validationBigramDice(
		[]rune(expectedNormalized),
		[]rune(actualNormalized),
	)
	lengthScore := validationLengthRatio(
		utf8.RuneCountInString(expectedNormalized),
		utf8.RuneCountInString(actualNormalized),
	)

	// Character similarity matters a little more for very short phrases, where
	// one inflected word would otherwise dominate the token score.
	if max(len(expectedTokens), len(actualTokens)) <= 3 {
		return clampUnit(0.45*tokenScore + 0.45*characterScore + 0.10*lengthScore)
	}
	return clampUnit(0.65*tokenScore + 0.25*characterScore + 0.10*lengthScore)
}

func validationTokenDice(left, right []string) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	counts := make(map[string]int, len(left))
	for _, token := range left {
		counts[token]++
	}
	common := 0
	for _, token := range right {
		if counts[token] <= 0 {
			continue
		}
		counts[token]--
		common++
	}
	return float64(2*common) / float64(len(left)+len(right))
}

func validationBigramDice(left, right []rune) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	if len(left) == 1 || len(right) == 1 {
		if string(left) == string(right) {
			return 1
		}
		return 0
	}
	encode := func(first, second rune) string {
		return string([]rune{first, second})
	}
	counts := make(map[string]int, len(left)-1)
	for index := 0; index+1 < len(left); index++ {
		counts[encode(left[index], left[index+1])]++
	}
	common := 0
	for index := 0; index+1 < len(right); index++ {
		key := encode(right[index], right[index+1])
		if counts[key] <= 0 {
			continue
		}
		counts[key]--
		common++
	}
	return float64(2*common) / float64((len(left)-1)+(len(right)-1))
}

func validationLengthRatio(left, right int) float64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	return float64(min(left, right)) / float64(max(left, right))
}

func clampUnit(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func transcriptTokenBudget(tokenCount int) int {
	budget := int(math.Ceil(float64(tokenCount) * 0.45))
	if budget < 1 {
		budget = 1
	}
	return min(budget, maxTranscriptTokenDistance)
}

func transcriptRuneBudget(runeCount int) int {
	if runeCount <= 0 {
		return 0
	}
	budget := int(math.Ceil(float64(runeCount) * 0.52))
	if budget < 1 {
		budget = 1
	}
	return min(budget, maxTranscriptRuneDistance)
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
	if !ok || total > math.MaxInt64-current {
		return 0, 0, false
	}
	return total + current, consumed, true
}

func criticalValidationTokens(tokens []string) []string {
	result := make([]string, 0)
	for _, token := range tokens {
		if strings.HasPrefix(token, "#") {
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

func boundedTokenEditStats(left, right []string, limit int) tokenEditStats {
	distance := boundedTokenEditDistance(left, right, limit)
	if distance > limit {
		return tokenEditStats{Distance: limit + 1}
	}
	insertions := max(0, len(right)-len(left))
	deletions := max(0, len(left)-len(right))
	return tokenEditStats{
		Distance:   distance,
		Insertions: insertions,
		Deletions:  deletions,
	}
}

func validationTokensSimilar(left, right string) bool {
	if left == right {
		return true
	}
	if strings.HasPrefix(left, "#") || strings.HasPrefix(right, "#") {
		return false
	}
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	longest := max(len(leftRunes), len(rightRunes))
	if longest < 5 {
		return false
	}
	budget := max(1, longest/4)
	return boundedRuneEditDistance(leftRunes, rightRunes, budget) <= budget
}

func tokenRuneCost(token string) int { return utf8.RuneCountInString(token) }

func boundedTokenEditDistance(left, right []string, limit int) int {
	return boundedEditDistance(
		len(left),
		len(right),
		limit,
		func(i, j int) bool {
			return left[i] == right[j] || validationTokensSimilar(left[i], right[j])
		},
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
