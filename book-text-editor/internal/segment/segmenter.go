// Package segment splits book text into TTS-friendly segments.
package segment

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultMaxWords is the preferred upper bound for a single segment.
const DefaultMaxWords = 60

// OmniVoiceHardMaxWords and OmniVoiceHardMaxRunes mirror the default request
// limits enforced by the local OmniVoice worker. They are hard safety limits,
// unlike DefaultMaxWords, which is a quality-oriented packing preference.
const (
	OmniVoiceHardMaxWords = 120
	OmniVoiceHardMaxRunes = 2_000
)

// Profile identifies a versioned segmentation algorithm. LegacyV1 remains the
// production default until ProsodyV2 has passed controlled listening tests.
type Profile string

const (
	LegacyV1  Profile = "legacy-v1"
	ProsodyV2 Profile = "prosody-v2"
)

const sceneBreak = "* * *"

// Warning describes a source phrase that cannot fit into the preferred limit
// without destroying its sentence boundary.
type Warning struct {
	Phrase string
	Words  int
	Limit  int
	Kind   string
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

// WarningHandler receives non-fatal diagnostics for phrases that are kept
// intact even though they exceed the preferred word limit.
type WarningHandler func(Warning)

// Segmenter splits text at semantic boundaries. The zero value is ready to use.
type Segmenter struct {
	maxWords  int
	hardWords int
	hardRunes int
	profile   Profile
	onWarning WarningHandler
}

// New creates a Segmenter with the requested preferred word limit.
func New(maxWords int) (Segmenter, error) {
	return NewWithProfile(LegacyV1, maxWords)
}

// NewWithProfile creates a versioned segmenter. ProsodyV2 preserves structural
// line boundaries, protects common Russian abbreviations and initials, and
// deterministically splits phrases that would be rejected by OmniVoice.
func NewWithProfile(profile Profile, maxWords int) (Segmenter, error) {
	if maxWords <= 0 {
		return Segmenter{}, fmt.Errorf("max words must be positive, got %d", maxWords)
	}
	if profile != LegacyV1 && profile != ProsodyV2 {
		return Segmenter{}, fmt.Errorf("unsupported segmentation profile %q", profile)
	}
	return Segmenter{
		maxWords:  maxWords,
		hardWords: OmniVoiceHardMaxWords,
		hardRunes: OmniVoiceHardMaxRunes,
		profile:   profile,
	}, nil
}

// WithWarningHandler returns a copy that reports non-fatal segmentation
// warnings. The handler is optional and is never called by SplitDetailed.
func (s Segmenter) WithWarningHandler(handler WarningHandler) Segmenter {
	s.onWarning = handler
	return s
}

// MaxWords returns the effective preferred word limit.
func (s Segmenter) MaxWords() int {
	if s.maxWords == 0 {
		return DefaultMaxWords
	}
	return s.maxWords
}

// Profile returns the effective versioned algorithm name.
func (s Segmenter) Profile() Profile {
	if s.profile == "" {
		return LegacyV1
	}
	return s.profile
}

// Split is the compatibility entry point used by the FB2 parser.
func (s Segmenter) Split(text string) []string {
	result := s.SplitDetailed(text)
	if s.onWarning != nil {
		for _, warning := range result.Warnings {
			s.onWarning(warning)
		}
	}
	return result.Segments
}

// SplitDetailed packs complete phrases up to MaxWords. A single phrase longer
// than the limit is kept intact and reported as a warning instead of being cut
// at an arbitrary word, which produces unnatural TTS and false STT warnings.
func (s Segmenter) SplitDetailed(text string) Result {
	units := splitIntoUnits(text)
	if s.Profile() == ProsodyV2 {
		units = splitIntoProsodyUnits(text)
	}
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

		if s.Profile() == ProsodyV2 && s.exceedsHardLimit(unit.text) {
			flush()
			parts, warning := s.splitAtHardLimit(unit.text)
			result.Segments = append(result.Segments, parts...)
			result.Warnings = append(result.Warnings, warning)
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
				Kind:   "soft_limit_preserved",
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

func (s Segmenter) exceedsHardLimit(text string) bool {
	return countWords(text) > s.hardWords || utf8.RuneCountInString(text) > s.hardRunes
}

func (s Segmenter) splitAtHardLimit(text string) ([]string, Warning) {
	original := text
	parts := make([]string, 0, 2)
	kind := "hard_limit_clause_split"
	for s.exceedsHardLimit(text) {
		cut, clauseBoundary := hardSplitIndex(text, s.hardWords, s.hardRunes)
		if cut <= 0 {
			cut = runeByteIndex(text, s.hardRunes)
			clauseBoundary = false
		}
		part := strings.TrimSpace(text[:cut])
		if part == "" {
			cut = runeByteIndex(text, s.hardRunes)
			part = text[:cut]
		}
		parts = append(parts, part)
		text = strings.TrimSpace(text[cut:])
		if !clauseBoundary {
			kind = "hard_limit_forced_split"
		}
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts, Warning{
		Phrase: original,
		Words:  countWords(original),
		Limit:  s.hardWords,
		Kind:   kind,
	}
}

func hardSplitIndex(text string, maxWords, maxRunes int) (int, bool) {
	words := 0
	inWord := false
	runesSeen := 0
	lastWhitespace := 0
	lastClause := 0
	previous := rune(0)
	for byteIndex, character := range text {
		if runesSeen >= maxRunes {
			break
		}
		runesSeen++
		if unicode.IsSpace(character) {
			inWord = false
			lastWhitespace = byteIndex
			if isClauseBoundary(previous) {
				lastClause = byteIndex
			}
			previous = character
			continue
		}
		if !inWord {
			words++
			inWord = true
			if words > maxWords {
				break
			}
		}
		previous = character
	}
	if lastClause > 0 {
		return lastClause, true
	}
	return lastWhitespace, false
}

func runeByteIndex(text string, runeLimit int) int {
	if runeLimit <= 0 {
		return 0
	}
	count := 0
	for byteIndex := range text {
		if count == runeLimit {
			return byteIndex
		}
		count++
	}
	return len(text)
}

func isClauseBoundary(character rune) bool {
	switch character {
	case ',', ';', ':', '—', '–':
		return true
	default:
		return false
	}
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

func splitIntoProsodyUnits(text string) []textUnit {
	var units []textUnit
	for _, line := range strings.Split(text, "\n") {
		line = normalizeSpace(line)
		if line == "" {
			continue
		}
		for {
			before, after, found := strings.Cut(line, sceneBreak)
			units = appendProsodyPhrases(units, before)
			if !found {
				break
			}
			units = appendBoundary(units)
			line = after
		}
		units = appendBoundary(units)
	}
	return units
}

func appendBoundary(units []textUnit) []textUnit {
	if len(units) == 0 || units[len(units)-1].forceBoundary {
		return units
	}
	return append(units, textUnit{forceBoundary: true})
}

func appendProsodyPhrases(units []textUnit, text string) []textUnit {
	text = strings.TrimSpace(text)
	if text == "" {
		return units
	}
	for _, phrase := range splitLineProsody(text) {
		units = append(units, textUnit{text: phrase})
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
	return splitLineWithPeriodRule(line, false)
}

func splitLineProsody(line string) []string {
	return splitLineWithPeriodRule(line, true)
}

func splitLineWithPeriodRule(line string, protectRussian bool) []string {
	runes := []rune(line)
	var phrases []string
	start := 0
	for index := 0; index < len(runes); index++ {
		if !isSentenceEnd(runes[index]) || isDecimalPoint(runes, index) ||
			(protectRussian && isProtectedRussianPeriod(runes, index)) {
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

var protectedRussianAbbreviations = map[string]struct{}{
	"г": {}, "гг": {}, "им": {}, "млн": {}, "млрд": {}, "рис": {},
	"см": {}, "стр": {}, "т": {}, "тыс": {}, "ул": {},
}

func isProtectedRussianPeriod(runes []rune, index int) bool {
	if runes[index] != '.' {
		return false
	}
	start := index
	for start > 0 && (unicode.IsLetter(runes[start-1]) || runes[start-1] == '-') {
		start--
	}
	token := strings.ToLower(string(runes[start:index]))
	if _, protected := protectedRussianAbbreviations[token]; protected {
		return true
	}
	if isCompoundAbbreviationTail(token) && previousDottedToken(runes, start) == "т" &&
		!nextNonSpaceIsUpper(runes, index+1) {
		return true
	}
	if utf8.RuneCountInString(token) != 1 || token == "я" {
		return false
	}
	next := index + 1
	for next < len(runes) && unicode.IsSpace(runes[next]) {
		next++
	}
	if next >= len(runes) || !unicode.IsUpper(runes[next]) {
		return false
	}
	// A single capital followed by another initial or a surname is an initial,
	// not a sentence. The explicit exclusion for "Я" protects the pronoun.
	return unicode.IsUpper(runes[start])
}

func nextNonSpaceIsUpper(runes []rune, start int) bool {
	for start < len(runes) && unicode.IsSpace(runes[start]) {
		start++
	}
	return start < len(runes) && unicode.IsUpper(runes[start])
}

func isCompoundAbbreviationTail(token string) bool {
	switch token {
	case "е", "к", "д", "п":
		return true
	default:
		return false
	}
}

func previousDottedToken(runes []rune, before int) string {
	index := before - 1
	for index >= 0 && unicode.IsSpace(runes[index]) {
		index--
	}
	if index < 0 || runes[index] != '.' {
		return ""
	}
	index--
	end := index + 1
	for index >= 0 && unicode.IsLetter(runes[index]) {
		index--
	}
	return strings.ToLower(string(runes[index+1 : end]))
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
