package quality

import (
	"slices"
	"strings"
	"testing"
)

func TestRepositoryCorpusIsValid(t *testing.T) {
	t.Parallel()
	corpus, err := LoadCorpus("../../../testdata/tts_quality/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	if got, wantMinimum := len(corpus.Cases), 30; got < wantMinimum {
		t.Fatalf("cases = %d, want at least %d", got, wantMinimum)
	}
}

func TestRepositoryCorpusCoversRequiredHomographs(t *testing.T) {
	t.Parallel()
	corpus, err := LoadCorpus("../../../testdata/tts_quality/corpus.json")
	if err != nil {
		t.Fatal(err)
	}

	required := []string{"атлас", "белки", "дорога", "замок", "мука", "орган", "плачу", "стоит", "стрелки", "уже"}
	var covered []string
	for _, testCase := range corpus.Cases {
		if testCase.Category != "homographs" {
			continue
		}
		for _, tag := range testCase.Tags {
			if slices.Contains(required, tag) {
				covered = append(covered, tag)
			}
		}
	}
	slices.Sort(covered)
	if !slices.Equal(covered, required) {
		t.Fatalf("covered homographs = %q, want %q", covered, required)
	}
}

func TestDecodeCorpusRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	input := `{"schema_version":"tts-quality-corpus-v1","corpus_version":"1.0.0","language":"ru-RU","description":"test","cases":[],"unexpected":true}`
	if _, err := DecodeCorpus(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("DecodeCorpus() error = %v, want unknown field error", err)
	}
}

func TestValidateRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()
	testCase := validCase("duplicate")
	corpus := validCorpus()
	corpus.Cases = append(corpus.Cases, testCase, testCase)
	if err := corpus.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("Validate() error = %v, want duplicate id error", err)
	}
}

func TestValidateRejectsNonNFCText(t *testing.T) {
	t.Parallel()
	testCase := validCase("non-nfc")
	testCase.SourceText = "е\u0308лка"
	corpus := validCorpus()
	corpus.Cases = append(corpus.Cases, testCase)
	if err := corpus.Validate(); err == nil || !strings.Contains(err.Error(), "Unicode NFC") {
		t.Fatalf("Validate() error = %v, want Unicode NFC error", err)
	}
}

func TestValidCorpusFixture(t *testing.T) {
	t.Parallel()
	if err := validCorpus().Validate(); err != nil {
		t.Fatalf("validCorpus().Validate() error = %v", err)
	}
}

func validCorpus() Corpus {
	corpus := Corpus{
		SchemaVersion: SupportedSchemaVersion,
		CorpusVersion: "test",
		Language:      "ru-RU",
		Description:   "test corpus",
	}
	for _, category := range requiredCategories {
		testCase := validCase(strings.ReplaceAll(category, "_", "-"))
		testCase.Category = category
		corpus.Cases = append(corpus.Cases, testCase)
	}
	return corpus
}

func validCase(id string) Case {
	return Case{
		ID:            id,
		Category:      id,
		BlockType:     "sentence",
		SourceText:    "Тест.",
		Tags:          []string{"test"},
		ReviewFocus:   []string{"pronunciation"},
		TextPreserved: true,
	}
}
