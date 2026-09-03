package api

import "book-text-editor/internal/russiantext"

type ttsTextPreparationReport struct {
	Version                  string
	ManualPronunciationRules int
	SelectiveStressRules     int
	MorphologyReplacements   int
}

func prepareRussianTTSText(
	source string,
	settings GenerationSettings,
) (string, ttsTextPreparationReport, error) {
	prepared, manualRules, err := applyPronunciationSettings(
		source,
		settings.Pronunciation,
	)
	if err != nil {
		return "", ttsTextPreparationReport{}, err
	}
	prepared, russianReport := russiantext.Prepare(prepared, russiantext.Options{
		SelectiveStress:     settings.RussianText.SelectiveStress,
		NormalizeMorphology: settings.RussianText.NormalizeMorphology,
	})
	return prepared, ttsTextPreparationReport{
		Version:                  russianReport.Version,
		ManualPronunciationRules: manualRules,
		SelectiveStressRules:     russianReport.SelectiveStressApplied,
		MorphologyReplacements:   russianReport.MorphologyReplacements,
	}, nil
}
