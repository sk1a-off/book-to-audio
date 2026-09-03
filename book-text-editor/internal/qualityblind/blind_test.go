package qualityblind

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildCreatesRevealSafeDeterministicPackage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	left := writeFixtureExperiment(t, root, "experiment-legacy", "legacy-v1", 42)
	right := writeFixtureExperiment(t, root, "experiment-prosody", "prosody-v2", 42)
	output := filepath.Join(root, "blind")
	createdAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	report, err := Build(Config{
		SessionID:   "segmentation-blind-001",
		ExperimentA: left,
		ExperimentB: right,
		OutputRoot:  output,
		ShuffleSeed: 99,
		CreatedAt:   createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.CaseCount != 2 || report.OutputDir != filepath.Join(output, report.SessionID) {
		t.Fatalf("report = %+v", report)
	}

	index := readFixtureFile(t, filepath.Join(report.OutputDir, "index.html"))
	session := readFixtureFile(t, filepath.Join(report.OutputDir, "session.json"))
	for _, hidden := range []string{"legacy-v1", "prosody-v2", "experiment-legacy", "experiment-prosody"} {
		if strings.Contains(index, hidden) || strings.Contains(session, hidden) {
			t.Fatalf("blind files reveal %q", hidden)
		}
	}
	for _, expected := range []string{
		"Слепое сравнение TTS",
		"Сохранить оценки JSON",
		"Произношение и ударения",
		"Просодия и паузы",
		"Сходство и стабильность голоса",
		"dimension_ratings",
		"tts-quality-blind-session-v1",
		"samples/case-one/A.wav",
		"samples/case-two/B.wav",
	} {
		if !strings.Contains(index+session, expected) {
			t.Errorf("blind package misses %q", expected)
		}
	}

	var reveal revealManifest
	decodeFixtureJSON(t, filepath.Join(report.OutputDir, "reveal.json"), &reveal)
	if reveal.SchemaVersion != RevealSchemaVersion || reveal.CreatedAt != createdAt ||
		reveal.PrimaryVariable != "segmenter_version" || len(reveal.Cases) != 2 {
		t.Fatalf("reveal = %+v", reveal)
	}
	for _, testCase := range reveal.Cases {
		if len(testCase.Mapping) != 2 {
			t.Fatalf("mapping for %s = %+v", testCase.CaseID, testCase.Mapping)
		}
		for _, label := range []string{"A", "B"} {
			revealed := testCase.Mapping[label]
			artifact := filepath.Join(report.OutputDir, "samples", testCase.CaseID, label+".wav")
			if got := sha256Hex(readFixtureBytes(t, artifact)); got != revealed.ArtifactSHA256 {
				t.Errorf("%s/%s hash = %s, want %s", testCase.CaseID, label, got, revealed.ArtifactSHA256)
			}
		}
	}

	second, err := Build(Config{
		SessionID:   "segmentation-blind-002",
		ExperimentA: left,
		ExperimentB: right,
		OutputRoot:  output,
		ShuffleSeed: 99,
		CreatedAt:   createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	var secondReveal revealManifest
	decodeFixtureJSON(t, filepath.Join(second.OutputDir, "reveal.json"), &secondReveal)
	if !reflect.DeepEqual(reveal.Cases, secondReveal.Cases) {
		t.Fatalf("same shuffle seed produced different mappings")
	}
}

func TestBuildRejectsNonControlledPair(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	left := writeFixtureExperiment(t, root, "experiment-legacy", "legacy-v1", 42)
	right := writeFixtureExperiment(t, root, "experiment-prosody", "prosody-v2", 43)
	_, err := Build(Config{
		SessionID:   "invalid-pair",
		ExperimentA: left,
		ExperimentB: right,
		OutputRoot:  filepath.Join(root, "blind"),
		ShuffleSeed: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "generation seed") {
		t.Fatalf("Build() error = %v, want generation seed mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "blind", "invalid-pair")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid pair published output: %v", statErr)
	}
}

func TestBuildCreatesControlledFadeComparison(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	left := writeFixtureExperiment(t, root, "fade-baseline", "legacy-v1", 42)
	right := writeFixtureExperiment(t, root, "fade-candidate", "legacy-v1", 42)
	setFixtureInferenceField(t, left, "fade_duration", 0.1)
	setFixtureInferenceField(t, right, "fade_duration", 0.03)

	report, err := Build(Config{
		SessionID:       "fade-blind-001",
		ExperimentA:     left,
		ExperimentB:     right,
		OutputRoot:      filepath.Join(root, "blind"),
		ShuffleSeed:     7,
		PrimaryVariable: FadeDurationVariable,
	})
	if err != nil {
		t.Fatal(err)
	}
	var reveal revealManifest
	decodeFixtureJSON(t, filepath.Join(report.OutputDir, "reveal.json"), &reveal)
	if reveal.PrimaryVariable != FadeDurationVariable || len(reveal.Cases) != 2 {
		t.Fatalf("reveal = %+v", reveal)
	}
	for _, testCase := range reveal.Cases {
		for _, sample := range testCase.Mapping {
			var inference map[string]any
			if err := json.Unmarshal(sample.InferenceConfig, &inference); err != nil {
				t.Fatal(err)
			}
			fade, ok := inference["fade_duration"].(float64)
			if !ok || fade != 0.1 && fade != 0.03 {
				t.Fatalf("inference config = %v", inference)
			}
		}
	}
}

func TestBuildRejectsFadePairWithAnotherInferenceDifference(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	left := writeFixtureExperiment(t, root, "fade-left", "legacy-v1", 42)
	right := writeFixtureExperiment(t, root, "fade-right", "legacy-v1", 42)
	setFixtureInferenceField(t, right, "fade_duration", 0.03)
	setFixtureInferenceField(t, right, "speed", 1.1)

	_, err := Build(Config{
		SessionID:       "invalid-fade-pair",
		ExperimentA:     left,
		ExperimentB:     right,
		OutputRoot:      filepath.Join(root, "blind"),
		PrimaryVariable: FadeDurationVariable,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the primary field") {
		t.Fatalf("Build() error = %v", err)
	}
}

func TestBuildCreatesControlledEngineComparison(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	left := writeFixtureExperiment(t, root, "omnivoice-baseline", "prosody-v2", 42)
	right := writeFixtureExperiment(t, root, "qwen-candidate", "prosody-v2", 42)
	setFixtureEngine(t, right, "Qwen3-TTS", "Qwen3-TTS-12Hz-1.7B-Base", "qwen-revision")
	setFixtureInferenceField(t, right, "temperature", 0.9)

	report, err := Build(Config{
		SessionID:       "engine-blind-001",
		ExperimentA:     left,
		ExperimentB:     right,
		OutputRoot:      filepath.Join(root, "blind"),
		ShuffleSeed:     17,
		PrimaryVariable: EngineVariable,
	})
	if err != nil {
		t.Fatal(err)
	}
	var reveal revealManifest
	decodeFixtureJSON(t, filepath.Join(report.OutputDir, "reveal.json"), &reveal)
	if reveal.PrimaryVariable != EngineVariable || len(reveal.Cases) != 2 {
		t.Fatalf("reveal = %+v", reveal)
	}
	for _, testCase := range reveal.Cases {
		engines := map[string]bool{}
		for _, sample := range testCase.Mapping {
			engines[sample.Engine] = true
			if sample.Model == "" || sample.ModelRevision == "" {
				t.Fatalf("missing model provenance: %+v", sample)
			}
		}
		if !engines["OmniVoice"] || !engines["Qwen3-TTS"] {
			t.Fatalf("engine mapping = %v", engines)
		}
	}
}

func TestBuildCreatesControlledNormalizerComparison(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	left := writeFixtureExperiment(t, root, "normalizer-baseline", "prosody-v2", 42)
	right := writeFixtureExperiment(t, root, "normalizer-candidate", "prosody-v2", 42)
	setFixtureNormalizer(t, right, "ru-selective-morph-v3:morphology", "подготовленный текст")

	report, err := Build(Config{
		SessionID:       "normalizer-blind-001",
		ExperimentA:     left,
		ExperimentB:     right,
		OutputRoot:      filepath.Join(root, "blind"),
		ShuffleSeed:     23,
		PrimaryVariable: NormalizerVariable,
	})
	if err != nil {
		t.Fatal(err)
	}
	var reveal revealManifest
	decodeFixtureJSON(t, filepath.Join(report.OutputDir, "reveal.json"), &reveal)
	if reveal.PrimaryVariable != NormalizerVariable || len(reveal.Cases) != 2 {
		t.Fatalf("reveal = %+v", reveal)
	}
	for _, testCase := range reveal.Cases {
		versions := map[string]bool{}
		for _, sample := range testCase.Mapping {
			if sample.NormalizerVersion == nil {
				versions["baseline"] = true
			} else {
				versions[*sample.NormalizerVersion] = true
			}
		}
		if !versions["baseline"] || !versions["ru-selective-morph-v3:morphology"] {
			t.Fatalf("normalizer mapping = %v", versions)
		}
	}
}

func writeFixtureExperiment(t *testing.T, root, experimentID, segmenter string, seed uint32) string {
	t.Helper()
	directory := filepath.Join(root, experimentID)
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id   string
		text string
	}{
		{"case-one", "Первая проверочная фраза."},
		{"case-two", "Вторая проверочная фраза."},
	}
	inference := map[string]any{"num_steps": 32, "speed": 1.0, "fade_duration": 0.1}
	experiment := map[string]any{
		"schema_version":             "tts-quality-experiment-v1",
		"experiment_id":              experimentID,
		"corpus_version":             "test-v1",
		"case_ids":                   []string{"case-one", "case-two"},
		"reference_sha256":           strings.Repeat("a", 64),
		"reference_text_sha256":      strings.Repeat("b", 64),
		"reference_text_file_sha256": strings.Repeat("c", 64),
		"seed":                       seed,
		"silence_threshold_dbfs":     -50,
		"inference_config":           inference,
		"segmenter_version":          segmenter,
		"segmenter_max_words":        12,
		"normalizer_version":         nil,
		"text_overrides_sha256":      nil,
	}
	writeFixtureJSON(t, filepath.Join(directory, "experiment.json"), experiment)
	for index, testCase := range cases {
		audio := []byte(experimentID + ":" + testCase.id + ":audio")
		artifactName := testCase.id + ".wav"
		if err := os.WriteFile(filepath.Join(directory, artifactName), audio, 0o640); err != nil {
			t.Fatal(err)
		}
		sample := map[string]any{
			"schema_version":           "tts-quality-sample-v1",
			"sample_id":                experimentID + ":" + testCase.id,
			"case_id":                  testCase.id,
			"corpus_version":           "test-v1",
			"source_text":              testCase.text,
			"tts_text":                 testCase.text + " " + segmenter,
			"normalizer_version":       nil,
			"segmenter_version":        segmenter,
			"engine":                   "OmniVoice",
			"model":                    "test-model",
			"model_revision":           "test-revision",
			"seed":                     seed,
			"inference_config":         inference,
			"speaker_reference_sha256": strings.Repeat("a", 64),
			"artifact": map[string]any{
				"relative_path":    artifactName,
				"sha256":           sha256Hex(audio),
				"duration_seconds": float64(index + 1),
			},
			"chunks": []map[string]string{{"text": testCase.text + " " + segmenter}},
		}
		writeFixtureJSON(t, filepath.Join(directory, testCase.id+".json"), sample)
	}
	return directory
}

func setFixtureInferenceField(t *testing.T, directory, field string, value any) {
	t.Helper()
	paths := []string{filepath.Join(directory, "experiment.json")}
	for _, caseID := range []string{"case-one", "case-two"} {
		paths = append(paths, filepath.Join(directory, caseID+".json"))
	}
	for _, path := range paths {
		var document map[string]any
		decodeFixtureJSON(t, path, &document)
		inference, ok := document["inference_config"].(map[string]any)
		if !ok {
			t.Fatalf("%s inference_config = %T", path, document["inference_config"])
		}
		inference[field] = value
		writeFixtureJSON(t, path, document)
	}
}

func setFixtureEngine(t *testing.T, directory, engine, model, revision string) {
	t.Helper()
	for _, caseID := range []string{"case-one", "case-two"} {
		path := filepath.Join(directory, caseID+".json")
		var document map[string]any
		decodeFixtureJSON(t, path, &document)
		document["engine"] = engine
		document["model"] = model
		document["model_revision"] = revision
		writeFixtureJSON(t, path, document)
	}
}

func setFixtureNormalizer(t *testing.T, directory, version, suffix string) {
	t.Helper()
	paths := []string{filepath.Join(directory, "experiment.json")}
	for _, caseID := range []string{"case-one", "case-two"} {
		paths = append(paths, filepath.Join(directory, caseID+".json"))
	}
	for _, path := range paths {
		var document map[string]any
		decodeFixtureJSON(t, path, &document)
		document["normalizer_version"] = version
		if _, sample := document["tts_text"]; sample {
			document["tts_text"] = document["source_text"].(string) + " " + suffix
			document["chunks"] = []map[string]string{{
				"text": document["tts_text"].(string),
			}}
		}
		writeFixtureJSON(t, path, document)
	}
}

func writeFixtureJSON(t *testing.T, path string, value any) {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o640); err != nil {
		t.Fatal(err)
	}
}

func decodeFixtureJSON(t *testing.T, path string, destination any) {
	t.Helper()
	if err := json.Unmarshal(readFixtureBytes(t, path), destination); err != nil {
		t.Fatal(err)
	}
}

func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	return string(readFixtureBytes(t, path))
}

func readFixtureBytes(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path) // #nosec G304 -- test-owned temporary path.
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
