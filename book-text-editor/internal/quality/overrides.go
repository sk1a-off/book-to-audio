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

const SupportedTextOverridesSchemaVersion = "tts-quality-text-overrides-v1"

var normalizerVersionPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// TextOverrides is an experiment-only source-to-TTS text mapping. The current
// schema deliberately permits only combining acute accents; broader Russian
// normalization needs a separate versioned transform and validation contract.
type TextOverrides struct {
	SchemaVersion     string         `json:"schema_version"`
	NormalizerVersion string         `json:"normalizer_version"`
	TransformKind     string         `json:"transform_kind"`
	Description       string         `json:"description"`
	Cases             []TextOverride `json:"cases"`
}

type TextOverride struct {
	CaseID  string `json:"case_id"`
	TTSText string `json:"tts_text"`
}

// TextOverrideValidationOptions contains experiment-only relaxations. The zero
// value is the production-safe policy used by every existing caller.
type TextOverrideValidationOptions struct {
	AllowDenseStress bool
}

func LoadTextOverrides(path string) (TextOverrides, error) {
	file, err := os.Open(path) // #nosec G304 -- the local quality CLI selects this versioned input.
	if err != nil {
		return TextOverrides{}, fmt.Errorf("open text overrides %q: %w", path, err)
	}
	overrides, decodeErr := DecodeTextOverrides(file)
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return TextOverrides{}, fmt.Errorf("read text overrides %q: %w", path, err)
	}
	return overrides, nil
}

func DecodeTextOverrides(reader io.Reader) (TextOverrides, error) {
	return DecodeTextOverridesWithOptions(reader, TextOverrideValidationOptions{})
}

func DecodeTextOverridesWithOptions(
	reader io.Reader,
	options TextOverrideValidationOptions,
) (TextOverrides, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var overrides TextOverrides
	if err := decoder.Decode(&overrides); err != nil {
		return TextOverrides{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return TextOverrides{}, err
	}
	if err := overrides.ValidateWithOptions(options); err != nil {
		return TextOverrides{}, err
	}
	return overrides, nil
}

func (overrides TextOverrides) Validate() error {
	return overrides.ValidateWithOptions(TextOverrideValidationOptions{})
}

func (overrides TextOverrides) ValidateWithOptions(
	options TextOverrideValidationOptions,
) error {
	var validationErrors []error
	if overrides.SchemaVersion != SupportedTextOverridesSchemaVersion {
		validationErrors = append(validationErrors, fmt.Errorf(
			"schema_version = %q, want %q",
			overrides.SchemaVersion,
			SupportedTextOverridesSchemaVersion,
		))
	}
	if !normalizerVersionPattern.MatchString(overrides.NormalizerVersion) {
		validationErrors = append(validationErrors, fmt.Errorf(
			"invalid normalizer_version %q",
			overrides.NormalizerVersion,
		))
	}
	if overrides.TransformKind != "stress_marks_only" {
		validationErrors = append(validationErrors, fmt.Errorf(
			"transform_kind = %q, want stress_marks_only",
			overrides.TransformKind,
		))
	}
	if strings.TrimSpace(overrides.Description) == "" {
		validationErrors = append(validationErrors, errors.New("description is required"))
	}
	if len(overrides.Cases) == 0 {
		validationErrors = append(validationErrors, errors.New("cases must not be empty"))
	}
	seen := make(map[string]struct{}, len(overrides.Cases))
	for index, textOverride := range overrides.Cases {
		if !caseIDPattern.MatchString(textOverride.CaseID) {
			validationErrors = append(validationErrors, fmt.Errorf(
				"cases[%d]: invalid case_id %q",
				index,
				textOverride.CaseID,
			))
		} else if _, duplicate := seen[textOverride.CaseID]; duplicate {
			validationErrors = append(validationErrors, fmt.Errorf(
				"cases[%d]: duplicate case_id %q",
				index,
				textOverride.CaseID,
			))
		}
		seen[textOverride.CaseID] = struct{}{}
		if textOverride.TTSText == "" ||
			strings.TrimSpace(textOverride.TTSText) != textOverride.TTSText {
			validationErrors = append(validationErrors, fmt.Errorf(
				"cases[%d]: tts_text must be non-empty without surrounding whitespace",
				index,
			))
			continue
		}
		if !norm.NFC.IsNormalString(textOverride.TTSText) {
			validationErrors = append(validationErrors, fmt.Errorf(
				"cases[%d]: tts_text must use Unicode NFC",
				index,
			))
		}
		if err := validateCombiningAcutes(textOverride.TTSText); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("cases[%d]: %w", index, err))
		}
		stressMarks := strings.Count(textOverride.TTSText, "\u0301")
		wordCount := len(strings.Fields(StripCombiningAcutes(textOverride.TTSText)))
		if stressMarks == 0 {
			validationErrors = append(validationErrors, fmt.Errorf(
				"cases[%d]: sparse stress override must add at least one mark",
				index,
			))
		} else if !options.AllowDenseStress && wordCount > 1 && stressMarks*2 >= wordCount {
			validationErrors = append(validationErrors, fmt.Errorf(
				"cases[%d]: stress marks are not sparse: %d marks for %d words",
				index,
				stressMarks,
				wordCount,
			))
		}
	}
	return errors.Join(validationErrors...)
}

func (overrides TextOverrides) ByCaseID() map[string]string {
	result := make(map[string]string, len(overrides.Cases))
	for _, textOverride := range overrides.Cases {
		result[textOverride.CaseID] = textOverride.TTSText
	}
	return result
}

func ValidateStressOverride(source, ttsText string) error {
	if err := validateCombiningAcutes(ttsText); err != nil {
		return err
	}
	if StripCombiningAcutes(ttsText) != source {
		return errors.New("stress override changed canonical source content")
	}
	return nil
}

func StripCombiningAcutes(value string) string {
	return strings.Map(func(character rune) rune {
		if character == '\u0301' {
			return -1
		}
		return character
	}, value)
}

func validateCombiningAcutes(value string) error {
	characters := []rune(value)
	for index, character := range characters {
		if character != '\u0301' {
			continue
		}
		if index == 0 || !isRussianVowel(characters[index-1]) {
			return errors.New("combining acute must follow a Russian vowel")
		}
		if index+1 < len(characters) && unicode.IsMark(characters[index+1]) {
			return errors.New("combining acute must not be followed by another mark")
		}
	}
	return nil
}

func isRussianVowel(character rune) bool {
	return strings.ContainsRune("аеёиоуыэюяАЕЁИОУЫЭЮЯ", character)
}
