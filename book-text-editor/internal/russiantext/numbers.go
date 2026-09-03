package russiantext

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type grammaticalForm int

const (
	masculineCardinal grammaticalForm = iota
	feminineCardinal
	masculineNominative
	feminineNominative
	neuterNominative
	masculineGenitive
	masculineDative
	masculinePrepositional
)

var (
	headingArabicPattern     = regexp.MustCompile(`(?i)(глава|книга)[ \t]+([0-9]{1,4})`)
	headingRomanPattern      = regexp.MustCompile(`(?i)(глава|книга)[ \t]+([IVXLCDM]{1,12})`)
	ordinalSuffixPattern     = regexp.MustCompile(`(?i)([0-9]{1,9})-(ый|ой|й|го|му|м|ая|я|ое|е)`)
	kilometresPerHourPattern = regexp.MustCompile(`([0-9]{1,9})[ \t]+км/ч`)
	kilogramPattern          = regexp.MustCompile(`([0-9]{1,9})[ \t]+кг`)
	percentPattern           = regexp.MustCompile(`([0-9]{1,9})[ \t]*%`)
	reachedPercentPattern    = regexp.MustCompile(`(?i)(достигла|достигло|достигли|достиг)([ \t]+)([0-9]{1,3})[ \t]*%`)
	kilometrePattern         = regexp.MustCompile(`([0-9]{1,9})[ \t]+км`)
	roubleSymbolPattern      = regexp.MustCompile(`([0-9]{1,9})[ \t]*₽`)
	dollarSymbolPattern      = regexp.MustCompile(`\$([0-9]{1,9})`)
	decimalPattern           = regexp.MustCompile(`([0-9]{1,9})([,.])([0-9]{1,3})`)
	groupedNumberPattern     = regexp.MustCompile(`([0-9]{1,3}(?:[ \x{00A0}][0-9]{3})+)([ \t]+)([А-Яа-яЁё]+)`)
	numberWithNounPattern    = regexp.MustCompile(`([0-9]{1,9})([ \t]+)([А-Яа-яЁё]+)`)
)

// cardinalNounForms is deliberately small. An unknown noun is left with its
// digits because the surrounding case cannot be inferred from the noun alone.
var cardinalNounForms = map[string]grammaticalForm{
	"стул": masculineCardinal, "стула": masculineCardinal,
	"стульев": masculineCardinal, "человек": masculineCardinal,
	"участник": masculineCardinal, "участника": masculineCardinal,
	"участников": masculineCardinal, "вопрос": masculineCardinal,
	"вопроса": masculineCardinal, "вопросов": masculineCardinal,
	"рубль": masculineCardinal, "рубля": masculineCardinal,
	"рублей": masculineCardinal, "копейка": feminineCardinal,
	"копейки": feminineCardinal, "копеек": feminineCardinal,
	"книга": feminineCardinal, "книги": feminineCardinal,
	"страница": feminineCardinal, "страницы": feminineCardinal,
	"минута": feminineCardinal, "минуты": feminineCardinal,
	"секунда": feminineCardinal, "секунды": feminineCardinal,
}

var decimalContextWords = map[string]struct{}{
	"коэффициент": {}, "коэффициента": {},
	"литр": {}, "литра": {}, "литров": {},
}

func normalizeMorphology(text string) (string, int) {
	total := 0
	text, count := replaceHeadings(text, headingArabicPattern, false)
	total += count
	text, count = replaceHeadings(text, headingRomanPattern, true)
	total += count
	text, count = replaceOrdinals(text)
	total += count
	text, count = replaceCountedUnit(text, kilometresPerHourPattern, "километр", "километра", "километров", " в час")
	total += count
	text, count = replaceCountedUnit(text, kilogramPattern, "килограмм", "килограмма", "килограммов", "")
	total += count
	text, count = replaceReachedPercents(text)
	total += count
	text, count = replaceCountedUnit(text, percentPattern, "процент", "процента", "процентов", "")
	total += count
	text, count = replaceCountedUnit(text, kilometrePattern, "километр", "километра", "километров", "")
	total += count
	text, count = replaceCountedUnit(text, roubleSymbolPattern, "рубль", "рубля", "рублей", "")
	total += count
	text, count = replaceDollarSymbols(text)
	total += count
	text, count = replaceDecimals(text)
	total += count
	text, count = replaceGroupedNumbers(text)
	total += count
	text, count = replaceNumbersWithNouns(text)
	total += count
	return text, total
}

func replaceHeadings(text string, pattern *regexp.Regexp, roman bool) (string, int) {
	count := 0
	result := pattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		var value int64
		var ok bool
		if roman {
			parsed, valid := parseRoman(parts[2])
			value, ok = int64(parsed), valid
		} else {
			value, ok = parseUnsigned(parts[2])
		}
		words, wordsOK := ordinalWords(value, feminineNominative)
		if !ok || !wordsOK {
			return match
		}
		count++
		return parts[1] + " " + words
	})
	return result, count
}

func replaceOrdinals(text string) (string, int) {
	count := 0
	result := ordinalSuffixPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := ordinalSuffixPattern.FindStringSubmatch(match)
		value, ok := parseUnsigned(parts[1])
		if !ok {
			return match
		}
		form := masculineNominative
		switch strings.ToLower(parts[2]) {
		case "го":
			form = masculineGenitive
		case "му":
			form = masculineDative
		case "м":
			form = masculinePrepositional
		case "ая", "я":
			form = feminineNominative
		case "ое", "е":
			form = neuterNominative
		}
		words, wordsOK := ordinalWords(value, form)
		if !wordsOK {
			return match
		}
		count++
		return words
	})
	return result, count
}

func replaceCountedUnit(text string, pattern *regexp.Regexp, one, few, many, suffix string) (string, int) {
	count := 0
	result := pattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		value, ok := parseUnsigned(parts[1])
		words, wordsOK := cardinalWords(value, masculineCardinal)
		if !ok || !wordsOK {
			return match
		}
		count++
		return words + " " + pluralForm(value, one, few, many) + suffix
	})
	return result, count
}

func replaceReachedPercents(text string) (string, int) {
	count := 0
	result := reachedPercentPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := reachedPercentPattern.FindStringSubmatch(match)
		value, ok := parseUnsigned(parts[3])
		words, wordsOK := genitiveCardinalWords(value)
		if !ok || !wordsOK {
			return match
		}
		count++
		return parts[1] + parts[2] + words + " процентов"
	})
	return result, count
}

func replaceDollarSymbols(text string) (string, int) {
	count := 0
	result := dollarSymbolPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := dollarSymbolPattern.FindStringSubmatch(match)
		value, ok := parseUnsigned(parts[1])
		words, wordsOK := cardinalWords(value, masculineCardinal)
		if !ok || !wordsOK {
			return match
		}
		count++
		return words + " " + pluralForm(value, "доллар", "доллара", "долларов")
	})
	return result, count
}

func replaceDecimals(text string) (string, int) {
	count := 0
	indices := decimalPattern.FindAllStringIndex(text, -1)
	if len(indices) == 0 {
		return text, 0
	}
	var result strings.Builder
	result.Grow(len(text) + len(indices)*16)
	last := 0
	for _, bounds := range indices {
		start, end := bounds[0], bounds[1]
		match := text[start:end]
		result.WriteString(text[last:start])
		if !safeDecimalContext(text, start, end) ||
			hasAdjacentDecimalRune(text, start, end) {
			result.WriteString(match)
			last = end
			continue
		}
		parts := decimalPattern.FindStringSubmatch(match)
		whole, wholeOK := parseUnsigned(parts[1])
		fraction, fractionOK := parseUnsigned(parts[3])
		wholeWords, wordsOK := cardinalWords(whole, feminineCardinal)
		fractionWords, fractionWordsOK := cardinalWords(fraction, feminineCardinal)
		if !wholeOK || !fractionOK || !wordsOK || !fractionWordsOK {
			result.WriteString(match)
			last = end
			continue
		}
		denominatorOne, denominatorMany := "десятая", "десятых"
		switch len(parts[3]) {
		case 2:
			denominatorOne, denominatorMany = "сотая", "сотых"
		case 3:
			denominatorOne, denominatorMany = "тысячная", "тысячных"
		}
		wholeForm := "целых"
		if whole%100 != 11 && whole%10 == 1 {
			wholeForm = "целая"
		}
		denominator := denominatorMany
		if fraction%100 != 11 && fraction%10 == 1 {
			denominator = denominatorOne
		}
		count++
		result.WriteString(wholeWords + " " + wholeForm + " " + fractionWords + " " + denominator)
		last = end
	}
	result.WriteString(text[last:])
	return result.String(), count
}

func replaceGroupedNumbers(text string) (string, int) {
	count := 0
	result := groupedNumberPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := groupedNumberPattern.FindStringSubmatch(match)
		digits := strings.NewReplacer(" ", "", "\u00a0", "").Replace(parts[1])
		value, ok := parseUnsigned(digits)
		form, supported := genderForNoun(parts[3])
		words, wordsOK := cardinalWords(value, form)
		if !ok || !supported || !wordsOK {
			return match
		}
		count++
		return words + parts[2] + parts[3]
	})
	return result, count
}

func replaceNumbersWithNouns(text string) (string, int) {
	count := 0
	result := numberWithNounPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := numberWithNounPattern.FindStringSubmatch(match)
		value, ok := parseUnsigned(parts[1])
		form, supported := genderForNoun(parts[3])
		words, wordsOK := cardinalWords(value, form)
		if !ok || !supported || !wordsOK {
			return match
		}
		count++
		return words + parts[2] + parts[3]
	})
	return result, count
}

func genderForNoun(noun string) (grammaticalForm, bool) {
	form, ok := cardinalNounForms[strings.ToLower(noun)]
	return form, ok
}

func safeDecimalContext(text string, start, end int) bool {
	previous := adjacentWord(text[:start], true)
	next := adjacentWord(text[end:], false)
	_, previousOK := decimalContextWords[previous]
	_, nextOK := decimalContextWords[next]
	return previousOK || nextOK
}

func adjacentWord(text string, reverse bool) string {
	runes := []rune(text)
	if reverse {
		index := len(runes) - 1
		for index >= 0 && unicode.IsSpace(runes[index]) {
			index--
		}
		end := index + 1
		for index >= 0 && unicode.IsLetter(runes[index]) {
			index--
		}
		return strings.ToLower(string(runes[index+1 : end]))
	}
	index := 0
	for index < len(runes) && unicode.IsSpace(runes[index]) {
		index++
	}
	start := index
	for index < len(runes) && unicode.IsLetter(runes[index]) {
		index++
	}
	return strings.ToLower(string(runes[start:index]))
}

func hasAdjacentDecimalRune(text string, start, end int) bool {
	if start > 0 {
		previous := text[start-1]
		if previous >= '0' && previous <= '9' {
			return true
		}
		if (previous == '.' || previous == ',') && start > 1 &&
			text[start-2] >= '0' && text[start-2] <= '9' {
			return true
		}
	}
	if end < len(text) {
		next := text[end]
		if next >= '0' && next <= '9' {
			return true
		}
		if (next == '.' || next == ',') && end+1 < len(text) &&
			text[end+1] >= '0' && text[end+1] <= '9' {
			return true
		}
	}
	return false
}

func cardinalWords(value int64, form grammaticalForm) (string, bool) {
	if value < 0 || value > 999_999_999 {
		return "", false
	}
	if value == 0 {
		return "ноль", true
	}
	parts := make([]string, 0, 12)
	if millions := value / 1_000_000; millions > 0 {
		parts = append(parts, belowThousand(millions, masculineCardinal)...)
		parts = append(parts, pluralForm(millions, "миллион", "миллиона", "миллионов"))
		value %= 1_000_000
	}
	if thousands := value / 1_000; thousands > 0 {
		if thousands != 1 {
			parts = append(parts, belowThousand(thousands, feminineCardinal)...)
		}
		parts = append(parts, pluralForm(thousands, "тысяча", "тысячи", "тысяч"))
		value %= 1_000
	}
	parts = append(parts, belowThousand(value, form)...)
	return strings.Join(parts, " "), true
}

func genitiveCardinalWords(value int64) (string, bool) {
	if value < 0 || value > 999 {
		return "", false
	}
	if value == 0 {
		return "ноля", true
	}
	hundreds := []string{"", "ста", "двухсот", "трехсот", "четырехсот", "пятисот", "шестисот", "семисот", "восьмисот", "девятисот"}
	tens := []string{"", "", "двадцати", "тридцати", "сорока", "пятидесяти", "шестидесяти", "семидесяти", "восьмидесяти", "девяноста"}
	teens := []string{"десяти", "одиннадцати", "двенадцати", "тринадцати", "четырнадцати", "пятнадцати", "шестнадцати", "семнадцати", "восемнадцати", "девятнадцати"}
	units := []string{"", "одного", "двух", "трёх", "четырёх", "пяти", "шести", "семи", "восьми", "девяти"}
	parts := make([]string, 0, 3)
	if value >= 100 {
		parts = append(parts, hundreds[value/100])
		value %= 100
	}
	if value >= 10 && value < 20 {
		parts = append(parts, teens[value-10])
		return strings.Join(parts, " "), true
	}
	if value >= 20 {
		parts = append(parts, tens[value/10])
		value %= 10
	}
	if value > 0 {
		parts = append(parts, units[value])
	}
	return strings.Join(parts, " "), true
}

func belowThousand(value int64, form grammaticalForm) []string {
	hundreds := []string{"", "сто", "двести", "триста", "четыреста", "пятьсот", "шестьсот", "семьсот", "восемьсот", "девятьсот"}
	tens := []string{"", "", "двадцать", "тридцать", "сорок", "пятьдесят", "шестьдесят", "семьдесят", "восемьдесят", "девяносто"}
	teens := []string{"десять", "одиннадцать", "двенадцать", "тринадцать", "четырнадцать", "пятнадцать", "шестнадцать", "семнадцать", "восемнадцать", "девятнадцать"}
	units := []string{"", "один", "два", "три", "четыре", "пять", "шесть", "семь", "восемь", "девять"}
	if form == feminineCardinal {
		units[1], units[2] = "одна", "две"
	}
	result := make([]string, 0, 3)
	if value >= 100 {
		result = append(result, hundreds[value/100])
		value %= 100
	}
	if value >= 10 && value < 20 {
		return append(result, teens[value-10])
	}
	if value >= 20 {
		result = append(result, tens[value/10])
		value %= 10
	}
	if value > 0 {
		result = append(result, units[value])
	}
	return result
}

var ordinalBase = map[int64]string{
	1: "первый", 2: "второй", 3: "третий", 4: "четвертый", 5: "пятый",
	6: "шестой", 7: "седьмой", 8: "восьмой", 9: "девятый", 10: "десятый",
	11: "одиннадцатый", 12: "двенадцатый", 13: "тринадцатый", 14: "четырнадцатый",
	15: "пятнадцатый", 16: "шестнадцатый", 17: "семнадцатый", 18: "восемнадцатый",
	19: "девятнадцатый", 20: "двадцатый", 30: "тридцатый", 40: "сороковой",
	50: "пятидесятый", 60: "шестидесятый", 70: "семидесятый", 80: "восьмидесятый",
	90: "девяностый", 100: "сотый", 200: "двухсотый", 300: "трехсотый",
	400: "четырехсотый", 500: "пятисотый", 600: "шестисотый",
	700: "семисотый", 800: "восьмисотый", 900: "девятисотый",
}

func ordinalWords(value int64, form grammaticalForm) (string, bool) {
	cardinal, ok := cardinalWords(value, masculineCardinal)
	if !ok || value == 0 {
		return "", false
	}
	terminal := value % 100
	if terminal == 0 || terminal >= 20 {
		terminal = value % 10
	}
	if terminal == 0 {
		terminal = value % 100
	}
	if terminal == 0 {
		terminal = value % 1000
	}
	base, ok := ordinalBase[terminal]
	if !ok {
		return "", false
	}
	lastSpace := strings.LastIndexByte(cardinal, ' ')
	prefix := ""
	if lastSpace >= 0 {
		prefix = cardinal[:lastSpace+1]
	}
	return prefix + inflectOrdinal(base, form), true
}

func inflectOrdinal(base string, form grammaticalForm) string {
	if form == masculineNominative {
		return base
	}
	if base == "третий" {
		switch form {
		case feminineNominative:
			return "третья"
		case neuterNominative:
			return "третье"
		case masculineGenitive:
			return "третьего"
		case masculineDative:
			return "третьему"
		case masculinePrepositional:
			return "третьем"
		}
	}
	if utf8.RuneCountInString(base) < 3 {
		return base
	}
	runes := []rune(base)
	stem := string(runes[:len(runes)-2])
	switch form {
	case feminineNominative:
		return stem + "ая"
	case neuterNominative:
		return stem + "ое"
	case masculineGenitive:
		return stem + "ого"
	case masculineDative:
		return stem + "ому"
	case masculinePrepositional:
		return stem + "ом"
	default:
		return base
	}
}

func pluralForm(value int64, one, few, many string) string {
	lastHundred := value % 100
	if lastHundred >= 11 && lastHundred <= 14 {
		return many
	}
	switch value % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}

func parseUnsigned(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil
}

func parseRoman(value string) (int, bool) {
	values := map[rune]int{'I': 1, 'V': 5, 'X': 10, 'L': 50, 'C': 100, 'D': 500, 'M': 1000}
	upper := strings.ToUpper(value)
	total := 0
	previous := 0
	for index := len([]rune(upper)) - 1; index >= 0; index-- {
		current := values[[]rune(upper)[index]]
		if current == 0 {
			return 0, false
		}
		if current < previous {
			total -= current
		} else {
			total += current
			previous = current
		}
	}
	if total < 1 || total > 3999 || romanCanonical(total) != upper {
		return 0, false
	}
	return total, true
}

func romanCanonical(value int) string {
	type pair struct {
		value int
		text  string
	}
	pairs := []pair{{1000, "M"}, {900, "CM"}, {500, "D"}, {400, "CD"}, {100, "C"}, {90, "XC"}, {50, "L"}, {40, "XL"}, {10, "X"}, {9, "IX"}, {5, "V"}, {4, "IV"}, {1, "I"}}
	var result strings.Builder
	for _, item := range pairs {
		for value >= item.value {
			result.WriteString(item.text)
			value -= item.value
		}
	}
	return result.String()
}

func capitalizeLikeSource(source, replacement string) string {
	first, _ := utf8.DecodeRuneInString(source)
	if !unicode.IsUpper(first) || replacement == "" {
		return replacement
	}
	replacementRunes := []rune(replacement)
	replacementRunes[0] = unicode.ToUpper(replacementRunes[0])
	return string(replacementRunes)
}
