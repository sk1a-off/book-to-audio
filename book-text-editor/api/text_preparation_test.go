package api

import (
	"strings"
	"testing"
)

func TestPrepareRussianTTSTextComposesManualStressAndMorphology(t *testing.T) {
	t.Parallel()
	settings := defaultGenerationSettings()
	settings.Pronunciation = PronunciationGenerationSettings{
		Enabled: true,
		Rules:   "особый замок => особый замо́к",
	}
	settings.RussianText.SelectiveStress = true
	settings.RussianText.NormalizeMorphology = true

	source := "Глава 7. Особый замок и старинный замок стоял на вершине холма. В комнате лежала 21 книга."
	got, report, err := prepareRussianTTSText(source, settings)
	if err != nil {
		t.Fatal(err)
	}
	want := "Глава седьмая. Особый замо́к и старинный за́мок стоял на вершине холма́. В комнате лежала двадцать одна книга."
	if got != want {
		t.Fatalf("prepareRussianTTSText() = %q, want %q", got, want)
	}
	if report.ManualPronunciationRules != 1 ||
		report.SelectiveStressRules != 2 ||
		report.MorphologyReplacements != 2 ||
		report.Version != "ru-selective-morph-v3" {
		t.Fatalf("report = %+v", report)
	}
	if strings.ContainsRune(source, combiningAcuteAccent) || strings.Contains(source, "седьмая") {
		t.Fatalf("source was unexpectedly changed: %q", source)
	}
}

func TestPrepareRussianTTSTextManualRuleWinsOverBuiltInRule(t *testing.T) {
	t.Parallel()
	settings := defaultGenerationSettings()
	settings.Pronunciation = PronunciationGenerationSettings{
		Enabled: true,
		Rules:   "старинный замок => старинный замо́к",
	}
	settings.RussianText.SelectiveStress = true

	got, report, err := prepareRussianTTSText("Старинный замок стоял у реки.", settings)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Старинный замо́к стоял у реки." ||
		report.ManualPronunciationRules != 1 || report.SelectiveStressRules != 0 {
		t.Fatalf("result = (%q, %+v)", got, report)
	}
}
