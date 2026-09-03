package quality

import (
	"strings"
	"testing"
)

func TestDecodeTextOverrides(t *testing.T) {
	t.Parallel()
	input := `{
      "schema_version":"tts-quality-text-overrides-v1",
      "normalizer_version":"curated-stress-v1",
      "transform_kind":"stress_marks_only",
      "description":"test",
      "cases":[{"case_id":"homograph-zamok","tts_text":"старый замо́к и высокий за́мок у дороги"}]
    }`
	overrides, err := DecodeTextOverrides(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got := overrides.ByCaseID()["homograph-zamok"]; got != "старый замо́к и высокий за́мок у дороги" {
		t.Fatalf("override = %q", got)
	}
}

func TestDecodeTextOverridesDenseStressRequiresExplicitOption(t *testing.T) {
	input := `{
		"schema_version":"tts-quality-text-overrides-v1",
		"normalizer_version":"dense-test-v1",
		"transform_kind":"stress_marks_only",
		"description":"controlled dense stress experiment",
		"cases":[{
			"case_id":"dense-case",
			"tts_text":"Ма́ма мы́ла ра́му"
		}]
	}`

	if _, err := DecodeTextOverrides(strings.NewReader(input)); err == nil ||
		!strings.Contains(err.Error(), "stress marks are not sparse") {
		t.Fatalf("DecodeTextOverrides() error = %v", err)
	}

	overrides, err := DecodeTextOverridesWithOptions(
		strings.NewReader(input),
		TextOverrideValidationOptions{AllowDenseStress: true},
	)
	if err != nil {
		t.Fatalf("DecodeTextOverridesWithOptions() error = %v", err)
	}
	if got := overrides.Cases[0].TTSText; got != "Ма́ма мы́ла ра́му" {
		t.Fatalf("tts_text = %q", got)
	}
}

func TestTextOverridesRejectUnsafeChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
	}{
		{name: "acute after consonant", text: "зам́ок"},
		{name: "duplicate mark", text: "замо́́к"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := `{
          "schema_version":"tts-quality-text-overrides-v1",
          "normalizer_version":"curated-stress-v1",
          "transform_kind":"stress_marks_only",
          "description":"test",
          "cases":[{"case_id":"homograph-zamok","tts_text":"` + test.text + `"}]
        }`
			if _, err := DecodeTextOverrides(strings.NewReader(input)); err == nil {
				t.Fatal("DecodeTextOverrides() error = nil")
			}
		})
	}
}

func TestTextOverridesRejectDenseStressMarkup(t *testing.T) {
	t.Parallel()
	input := `{
      "schema_version":"tts-quality-text-overrides-v1",
      "normalizer_version":"curated-stress-v1",
      "transform_kind":"stress_marks_only",
      "description":"test",
      "cases":[{"case_id":"homograph-zamok","tts_text":"за́мок замо́к"}]
    }`
	if _, err := DecodeTextOverrides(strings.NewReader(input)); err == nil ||
		!strings.Contains(err.Error(), "not sparse") {
		t.Fatalf("DecodeTextOverrides() error = %v", err)
	}
}

func TestValidateStressOverridePreservesCanonicalSource(t *testing.T) {
	t.Parallel()
	if err := ValidateStressOverride("замок и замок", "замо́к и за́мок"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStressOverride("замок", "звонок"); err == nil {
		t.Fatal("changed word accepted")
	}
}

func TestRepositoryStressOverridesMatchCorpus(t *testing.T) {
	t.Parallel()
	corpus, err := LoadCorpus("../../../testdata/tts_quality/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]string, len(corpus.Cases))
	for _, testCase := range corpus.Cases {
		sources[testCase.ID] = testCase.SourceText
	}
	files := map[string]int{
		"stress-overrides-v1.json":          10,
		"stress-overrides-accepted-v1.json": 8,
	}
	for name, expectedCases := range files {
		overrides, err := LoadTextOverrides(
			"../../../testdata/tts_quality/experiments/" + name,
		)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(overrides.Cases) != expectedCases {
			t.Errorf("%s cases=%d, want %d", name, len(overrides.Cases), expectedCases)
		}
		for caseID, ttsText := range overrides.ByCaseID() {
			source, ok := sources[caseID]
			if !ok {
				t.Errorf("%s: unknown case %q", name, caseID)
				continue
			}
			if err := ValidateStressOverride(source, ttsText); err != nil {
				t.Errorf("%s case %q: %v", name, caseID, err)
			}
		}
	}
}
