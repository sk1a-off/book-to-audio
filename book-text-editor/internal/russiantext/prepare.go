// Package russiantext prepares a separate Russian TTS representation while
// leaving the canonical book text untouched.
package russiantext

// Version identifies the exact deterministic preparation rules. It is stored
// with a generation job so a future ruleset change cannot silently alter a
// resumed book.
const Version = "ru-selective-morph-v3"

// Options enables independent, experiment-gated transformations.
type Options struct {
	SelectiveStress     bool
	NormalizeMorphology bool
}

// Report describes only transformations actually applied to the TTS text.
type Report struct {
	Version                string
	SelectiveStressApplied int
	MorphologyReplacements int
}

// Prepare creates an OmniVoice-specific text representation. The function is
// deterministic and does not mutate or retain the source string.
func Prepare(source string, options Options) (string, Report) {
	prepared := source
	report := Report{Version: Version}
	if options.SelectiveStress {
		prepared, report.SelectiveStressApplied = applySelectiveStress(prepared)
	}
	if options.NormalizeMorphology {
		prepared, report.MorphologyReplacements = normalizeMorphology(prepared)
	}
	return prepared, report
}
