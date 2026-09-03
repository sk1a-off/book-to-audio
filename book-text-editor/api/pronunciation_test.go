package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplyPronunciationSettingsUsesContextForHomographs(t *testing.T) {
	t.Parallel()

	source := "Старинный замок стоял на вершине холма и был закрыт на дверной замок."
	settings := PronunciationGenerationSettings{
		Enabled: true,
		Rules: strings.Join([]string{
			"старинный замок => старинный за́мок",
			"вершине холма => вершине холма́",
			"дверной замок => дверной замо́к",
		}, "\n"),
	}

	got, applied, err := applyPronunciationSettings(source, settings)
	if err != nil {
		t.Fatalf("applyPronunciationSettings() error = %v", err)
	}
	want := "Старинный за́мок стоял на вершине холма́ и был закрыт на дверной замо́к."
	if got != want || applied != 3 {
		t.Fatalf("applyPronunciationSettings() = (%q, %d), want (%q, 3)", got, applied, want)
	}
	if canonical := strings.ReplaceAll(got, "\u0301", ""); canonical != source {
		t.Fatalf("canonical TTS text = %q, want unchanged source %q", canonical, source)
	}
}

func TestApplyPronunciationSettingsPrefersLongestRuleAndPreservesCase(t *testing.T) {
	t.Parallel()

	settings := PronunciationGenerationSettings{
		Enabled: true,
		Rules: strings.Join([]string{
			"замок => замо́к",
			"старинный замок => старинный за́мок",
		}, "\n"),
	}
	got, applied, err := applyPronunciationSettings(
		"СТАРИННЫЙ ЗАМОК и замокнуть.",
		settings,
	)
	if err != nil {
		t.Fatalf("applyPronunciationSettings() error = %v", err)
	}
	if want := "СТАРИННЫЙ ЗА́МОК и замокнуть."; got != want || applied != 1 {
		t.Fatalf("applyPronunciationSettings() = (%q, %d), want (%q, 1)", got, applied, want)
	}
}

func TestApplyPronunciationSettingsDisabled(t *testing.T) {
	t.Parallel()

	const source = "Старинный замок."
	got, applied, err := applyPronunciationSettings(source, PronunciationGenerationSettings{
		Rules: "замок => замо́к",
	})
	if err != nil || got != source || applied != 0 {
		t.Fatalf("applyPronunciationSettings() = (%q, %d, %v)", got, applied, err)
	}
}

func TestParsePronunciationRulesRejectsUnsafeChanges(t *testing.T) {
	t.Parallel()

	cases := []string{
		"замок - замо́к",
		"замок => замок",
		"замок => дворец",
		"замок => за́мок\nЗАМОК => замо́к",
		"замок => зам́ок",
		"за́мок => за́мок",
	}
	for _, rules := range cases {
		if _, err := parsePronunciationRules(rules); err == nil {
			t.Errorf("parsePronunciationRules(%q) error = nil", rules)
		}
	}
}

func TestDecodeGenerationSettingsPronunciationSnapshot(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(
		http.MethodPost,
		"/generate",
		strings.NewReader(`{"pronunciation":{"enabled":true,"rules":"замок => замо́к"}}`),
	)
	response := httptest.NewRecorder()
	got, ok := decodeGenerationSettings(response, request)
	if !ok || response.Code != http.StatusOK {
		t.Fatalf("decodeGenerationSettings() = (%+v, %v), status %d", got, ok, response.Code)
	}
	if !got.Pronunciation.Enabled || got.Pronunciation.Rules != "замок => замо́к" {
		t.Fatalf("pronunciation snapshot = %+v", got.Pronunciation)
	}
}

func TestGenerationUsesPronunciationOnlyForTTS(t *testing.T) {
	t.Parallel()

	settings := defaultGenerationSettings()
	settings.Pronunciation = PronunciationGenerationSettings{
		Enabled: true,
		Rules:   "Тестовый фрагмент => Тесто́вый фрагмент",
	}
	result := runRetryTest(t, settings, &retryTestTTS{}, &retryTestSTT{})
	if len(result.tts.requests) != 1 || len(result.stt.requests) != 1 {
		t.Fatalf("worker requests = TTS:%d STT:%d, want 1 each", len(result.tts.requests), len(result.stt.requests))
	}
	if got := result.tts.requests[0].Text; got != "Тесто́вый фрагмент." {
		t.Fatalf("TTS text = %q, want selective stress", got)
	}
	if got := result.stt.requests[0].ExpectedText; got != "Тестовый фрагмент." {
		t.Fatalf("STT expected text = %q, want canonical source", got)
	}
	if result.fragment.Text != "Тестовый фрагмент." {
		t.Fatalf("stored fragment text = %q, want canonical source", result.fragment.Text)
	}
}
