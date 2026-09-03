// Package quality defines versioned inputs for reproducible TTS quality checks.
package quality

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const SupportedSchemaVersion = "tts-quality-corpus-v1"

var caseIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

var allowedBlockTypes = map[string]struct{}{
	"chapter_transition": {},
	"dialogue":           {},
	"epigraph":           {},
	"heading":            {},
	"paragraph":          {},
	"poetry":             {},
	"sentence":           {},
}

var requiredCategories = []string{
	"abbreviations",
	"chapter_transition",
	"dates",
	"dialogue",
	"dialogue_author_words",
	"ellipsis",
	"emotional_dialogue",
	"epigraph",
	"fantasy_names",
	"foreign_words",
	"heading",
	"homographs",
	"initials",
	"money",
	"names",
	"nested_quotes",
	"neutral_narration",
	"no_final_punctuation",
	"numbers",
	"poetry",
	"prose",
	"question_exclamation",
	"roman_numerals",
	"units",
	"very_long_paragraph",
	"very_long_sentence",
}

type Corpus struct {
	SchemaVersion string `json:"schema_version"`
	CorpusVersion string `json:"corpus_version"`
	Language      string `json:"language"`
	Description   string `json:"description"`
	Cases         []Case `json:"cases"`
}

type Case struct {
	ID            string   `json:"id"`
	Category      string   `json:"category"`
	BlockType     string   `json:"block_type"`
	SourceText    string   `json:"source_text"`
	Tags          []string `json:"tags"`
	ReviewFocus   []string `json:"review_focus"`
	TextPreserved bool     `json:"text_preserved"`
}

func LoadCorpus(path string) (Corpus, error) {
	file, err := os.Open(path) // #nosec G304 -- the caller deliberately selects a local corpus.
	if err != nil {
		return Corpus{}, fmt.Errorf("open quality corpus %q: %w", path, err)
	}
	corpus, decodeErr := DecodeCorpus(file)
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return Corpus{}, fmt.Errorf("read quality corpus %q: %w", path, err)
	}
	return corpus, nil
}

func DecodeCorpus(reader io.Reader) (Corpus, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	var corpus Corpus
	if err := decoder.Decode(&corpus); err != nil {
		return Corpus{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Corpus{}, err
	}
	if err := corpus.Validate(); err != nil {
		return Corpus{}, err
	}
	return corpus, nil
}

func (corpus Corpus) Validate() error {
	var validationErrors []error
	if corpus.SchemaVersion != SupportedSchemaVersion {
		validationErrors = append(validationErrors, fmt.Errorf(
			"schema_version = %q, want %q",
			corpus.SchemaVersion,
			SupportedSchemaVersion,
		))
	}
	if strings.TrimSpace(corpus.CorpusVersion) == "" {
		validationErrors = append(validationErrors, errors.New("corpus_version is required"))
	}
	if corpus.Language != "ru-RU" {
		validationErrors = append(validationErrors, fmt.Errorf("language = %q, want ru-RU", corpus.Language))
	}
	if strings.TrimSpace(corpus.Description) == "" {
		validationErrors = append(validationErrors, errors.New("description is required"))
	}
	if len(corpus.Cases) == 0 {
		validationErrors = append(validationErrors, errors.New("cases must not be empty"))
	}

	seenIDs := make(map[string]struct{}, len(corpus.Cases))
	categoryCounts := make(map[string]int)
	for index, testCase := range corpus.Cases {
		if err := validateCase(testCase, seenIDs); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("cases[%d]: %w", index, err))
		}
		categoryCounts[testCase.Category]++
	}
	for _, category := range requiredCategories {
		if categoryCounts[category] == 0 {
			validationErrors = append(validationErrors, fmt.Errorf("required category %q is missing", category))
		}
	}
	return errors.Join(validationErrors...)
}

func validateCase(testCase Case, seenIDs map[string]struct{}) error {
	var validationErrors []error
	if !caseIDPattern.MatchString(testCase.ID) {
		validationErrors = append(validationErrors, fmt.Errorf("invalid id %q", testCase.ID))
	} else if _, exists := seenIDs[testCase.ID]; exists {
		validationErrors = append(validationErrors, fmt.Errorf("duplicate id %q", testCase.ID))
	} else {
		seenIDs[testCase.ID] = struct{}{}
	}
	if strings.TrimSpace(testCase.Category) == "" {
		validationErrors = append(validationErrors, errors.New("category is required"))
	}
	if _, ok := allowedBlockTypes[testCase.BlockType]; !ok {
		validationErrors = append(validationErrors, fmt.Errorf("unsupported block_type %q", testCase.BlockType))
	}
	if testCase.SourceText == "" || strings.TrimSpace(testCase.SourceText) != testCase.SourceText {
		validationErrors = append(validationErrors, errors.New("source_text must be non-empty without surrounding whitespace"))
	} else {
		if !norm.NFC.IsNormalString(testCase.SourceText) {
			validationErrors = append(validationErrors, errors.New("source_text must use Unicode NFC"))
		}
		if containsUnsupportedControl(testCase.SourceText) {
			validationErrors = append(validationErrors, errors.New("source_text contains an unsupported control character"))
		}
	}
	if !testCase.TextPreserved {
		validationErrors = append(validationErrors, errors.New("text_preserved must be true"))
	}
	if len(testCase.ReviewFocus) == 0 {
		validationErrors = append(validationErrors, errors.New("review_focus must not be empty"))
	}
	if duplicates := duplicateStrings(testCase.Tags); len(duplicates) > 0 {
		validationErrors = append(validationErrors, fmt.Errorf("duplicate tags: %s", strings.Join(duplicates, ", ")))
	}
	if duplicates := duplicateStrings(testCase.ReviewFocus); len(duplicates) > 0 {
		validationErrors = append(validationErrors, fmt.Errorf("duplicate review_focus values: %s", strings.Join(duplicates, ", ")))
	}
	return errors.Join(validationErrors...)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}

func containsUnsupportedControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return true
		}
	}
	return false
}

func duplicateStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var duplicates []string
	for _, value := range values {
		if _, ok := seen[value]; ok {
			duplicates = append(duplicates, value)
			continue
		}
		seen[value] = struct{}{}
	}
	return duplicates
}
