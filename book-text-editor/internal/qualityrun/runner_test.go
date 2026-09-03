package qualityrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"book-text-editor/api"
	"book-text-editor/internal/quality"
	"book-text-editor/internal/russiantext"
	"book-text-editor/internal/segment"
)

type recordingGenerator struct {
	requests []api.TTSRequest
}

func (generator *recordingGenerator) Generate(
	_ context.Context,
	request api.TTSRequest,
) (api.TTSResult, error) {
	generator.requests = append(generator.requests, request)
	pcm := make([]byte, 480)
	for index := range pcm {
		pcm[index] = byte(len(generator.requests))
	}
	digest := sha256.Sum256(pcm)
	cacheHit := true
	seed := *request.Seed
	return api.TTSResult{
		RequestID:            request.RequestID,
		AudioPCM:             pcm,
		SampleRate:           24_000,
		Channels:             1,
		SampleWidth:          2,
		DurationMS:           10,
		GenerationDurationMS: 5,
		SeedUsed:             &seed,
		VoiceCacheHit:        &cacheHit,
		ModelID:              "fake/omnivoice",
		ModelVersion:         "test-revision",
		AudioSHA256:          hex.EncodeToString(digest[:]),
	}, nil
}

func TestProsodyV2RunRecordsAndConcatenatesIndependentChunks(t *testing.T) {
	t.Parallel()
	generator := &recordingGenerator{}
	output := t.TempDir()
	report, err := Run(context.Background(), generator, testConfig(
		output,
		"prosody-chunks",
		segment.ProsodyV2,
	))
	if err != nil {
		t.Fatal(err)
	}
	if report.SampleCount != 1 || len(generator.requests) != 2 {
		t.Fatalf("report=%+v requests=%d, want one sample and two chunks", report, len(generator.requests))
	}
	if got := []uint32{*generator.requests[0].Seed, *generator.requests[1].Seed}; got[0] != 42 || got[1] != 43 {
		t.Fatalf("chunk seeds=%v, want [42 43]", got)
	}

	manifestPath := filepath.Join(output, "prosody-chunks", "chapter-transition-scene.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SourceText       string          `json:"source_text"`
		TTSText          string          `json:"tts_text"`
		SegmenterVersion string          `json:"segmenter_version"`
		Chunks           []chunkManifest `json:"chunks"`
		Artifact         struct {
			WorkerSHA *string `json:"worker_reported_pcm_sha256"`
			PCMSHA    string  `json:"pcm_sha256"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SegmenterVersion != string(segment.ProsodyV2) || len(manifest.Chunks) != 2 {
		t.Fatalf("manifest segmenter/chunks = %q/%d", manifest.SegmenterVersion, len(manifest.Chunks))
	}
	if compactWhitespace(manifest.SourceText) != compactWhitespace(manifest.TTSText) {
		t.Fatalf("text changed: source=%q tts=%q", manifest.SourceText, manifest.TTSText)
	}
	if manifest.Artifact.WorkerSHA != nil || manifest.Artifact.PCMSHA == "" {
		t.Fatalf("aggregate hashes = %+v", manifest.Artifact)
	}
}

func TestProsodyV2RunAddsAndRecordsAdaptiveMinimumPause(t *testing.T) {
	t.Parallel()
	generator := &recordingGenerator{}
	output := t.TempDir()
	config := testConfig(output, "adaptive-pause", segment.ProsodyV2)
	config.InterChunkMinPauseMS = 500
	report, err := Run(context.Background(), generator, config)
	if err != nil {
		t.Fatal(err)
	}
	if report.AudioSeconds != 0.52 {
		t.Fatalf("audio seconds = %v, want 0.52", report.AudioSeconds)
	}

	manifestPath := filepath.Join(output, "adaptive-pause", "chapter-transition-scene.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		AudioAssembly  audioAssemblyManifest `json:"audio_assembly"`
		WorkerResponse struct {
			AudioDurationMS int `json:"audio_duration_ms"`
		} `json:"worker_response"`
		Artifact struct {
			DurationSeconds float64 `json:"duration_seconds"`
		} `json:"artifact"`
		Chunks []chunkManifest `json:"chunks"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.AudioAssembly.Policy != "minimum-existing-silence-aware-v1" ||
		manifest.AudioAssembly.InterChunkMinPauseMS != 500 {
		t.Fatalf("audio assembly = %+v", manifest.AudioAssembly)
	}
	if manifest.WorkerResponse.AudioDurationMS != 20 || manifest.Artifact.DurationSeconds != 0.52 {
		t.Fatalf("durations = worker %d ms, artifact %.3f s",
			manifest.WorkerResponse.AudioDurationMS,
			manifest.Artifact.DurationSeconds,
		)
	}
	if len(manifest.Chunks) != 2 || manifest.Chunks[0].BoundaryBefore != nil ||
		manifest.Chunks[1].BoundaryBefore == nil {
		t.Fatalf("chunk boundaries = %+v", manifest.Chunks)
	}
	boundary := manifest.Chunks[1].BoundaryBefore
	if boundary.TargetSeconds != 0.5 || boundary.ExistingSeconds != 0 ||
		boundary.AddedSeconds != 0.5 || boundary.ResultingSeconds != 0.5 {
		t.Fatalf("boundary = %+v", boundary)
	}
}

func TestProsodyV2RunAppliesExplicitPauseToEachBoundary(t *testing.T) {
	t.Parallel()
	generator := &recordingGenerator{}
	output := t.TempDir()
	config := testConfig(output, "explicit-boundary-pauses", segment.ProsodyV2)
	config.CaseIDs = []string{"heading-chapter-seven-words"}
	config.SegmenterMaxWords = 5
	config.BoundaryMinPausesMS = []int{800, 500}
	report, err := Run(context.Background(), generator, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(generator.requests) != 3 || report.AudioSeconds != 1.33 {
		t.Fatalf("requests=%d report=%+v, want three chunks and 1.33s", len(generator.requests), report)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(
		output,
		"explicit-boundary-pauses",
		"heading-chapter-seven-words.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		AudioAssembly audioAssemblyManifest `json:"audio_assembly"`
		Chunks        []chunkManifest       `json:"chunks"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.AudioAssembly.Policy !=
		"minimum-existing-silence-aware-explicit-boundaries-v1" ||
		!slices.Equal(manifest.AudioAssembly.BoundaryMinPausesMS, []int{800, 500}) {
		t.Fatalf("audio assembly = %+v", manifest.AudioAssembly)
	}
	if len(manifest.Chunks) != 3 || manifest.Chunks[1].BoundaryBefore == nil ||
		manifest.Chunks[2].BoundaryBefore == nil ||
		math.Abs(manifest.Chunks[1].BoundaryBefore.TargetSeconds-0.8) > 1e-9 ||
		math.Abs(manifest.Chunks[2].BoundaryBefore.TargetSeconds-0.5) > 1e-9 {
		t.Fatalf("chunks = %+v", manifest.Chunks)
	}
}

func TestRunRejectsExplicitPauseCountThatDoesNotMatchBoundaries(t *testing.T) {
	t.Parallel()
	config := testConfig(t.TempDir(), "bad-boundary-count", segment.ProsodyV2)
	config.BoundaryMinPausesMS = []int{800, 500}
	_, err := Run(context.Background(), &recordingGenerator{}, config)
	if err == nil || !strings.Contains(err.Error(), "does not match 1 generated boundaries") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestLegacyV1RunKeepsCurrentSingleChunkBehaviour(t *testing.T) {
	t.Parallel()
	generator := &recordingGenerator{}
	_, err := Run(context.Background(), generator, testConfig(
		t.TempDir(),
		"legacy-single-chunk",
		segment.LegacyV1,
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(generator.requests) != 1 {
		t.Fatalf("legacy requests=%d, want 1", len(generator.requests))
	}
}

func TestRunKeepsCanonicalSourceSeparateFromStressMarkedTTSText(t *testing.T) {
	t.Parallel()
	generator := &recordingGenerator{}
	config := testConfig(t.TempDir(), "stress-override", segment.LegacyV1)
	config.CaseIDs = []string{"homograph-zamok"}
	config.TextOverrides = &quality.TextOverrides{
		SchemaVersion:     quality.SupportedTextOverridesSchemaVersion,
		NormalizerVersion: "curated-stress-v1",
		TransformKind:     "stress_marks_only",
		Description:       "test",
		Cases: []quality.TextOverride{{
			CaseID: "homograph-zamok",
			TTSText: "Старинный замо́к на двери не открывался, пока путники " +
				"рассматривали за́мок на вершине холма.",
		}},
	}
	config.TextOverridesPath = "stress-overrides.json"
	config.TextOverridesSHA256 = strings.Repeat("a", 64)
	_, err := Run(context.Background(), generator, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(generator.requests) != 1 || !strings.Contains(generator.requests[0].Text, "о́") {
		t.Fatalf("worker requests = %+v", generator.requests)
	}
	manifestPath := filepath.Join(
		config.OutputRoot,
		config.ExperimentID,
		"homograph-zamok.json",
	)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SourceText        string  `json:"source_text"`
		TTSText           string  `json:"tts_text"`
		NormalizerVersion *string `json:"normalizer_version"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(manifest.SourceText, '\u0301') ||
		!strings.ContainsRune(manifest.TTSText, '\u0301') ||
		manifest.NormalizerVersion == nil || *manifest.NormalizerVersion != "curated-stress-v1" {
		t.Fatalf("manifest source/TTS separation failed: %+v", manifest)
	}
}

func TestRunKeepsCanonicalSourceSeparateFromBuiltInRussianPreparation(t *testing.T) {
	t.Parallel()
	generator := &recordingGenerator{}
	config := testConfig(t.TempDir(), "russian-preparation", segment.LegacyV1)
	config.CaseIDs = []string{"chapter-transition-scene"}
	config.RussianTextOptions = russiantext.Options{
		SelectiveStress:     true,
		NormalizeMorphology: true,
	}
	_, err := Run(context.Background(), generator, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(generator.requests) != 1 ||
		!strings.Contains(generator.requests[0].Text, "Глава седьмая") {
		t.Fatalf("worker requests = %+v", generator.requests)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(
		config.OutputRoot,
		config.ExperimentID,
		"chapter-transition-scene.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SourceText        string  `json:"source_text"`
		TTSText           string  `json:"tts_text"`
		NormalizerVersion *string `json:"normalizer_version"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(manifest.SourceText, "седьмая") ||
		!strings.Contains(manifest.TTSText, "седьмая") ||
		manifest.NormalizerVersion == nil ||
		*manifest.NormalizerVersion != "ru-selective-morph-v3:stress+morphology" {
		t.Fatalf("manifest source/TTS separation failed: %+v", manifest)
	}
}

func TestRunRejectsTextOverridesCombinedWithBuiltInPreparation(t *testing.T) {
	t.Parallel()
	config := testConfig(t.TempDir(), "mixed-normalizers", segment.LegacyV1)
	config.TextOverrides = &quality.TextOverrides{}
	config.TextOverridesPath = "overrides.json"
	config.TextOverridesSHA256 = strings.Repeat("a", 64)
	config.RussianTextOptions.SelectiveStress = true
	_, err := Run(context.Background(), &recordingGenerator{}, config)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestProsodyV2FullCorpusConservesTextAndRespectsWorkerLimits(t *testing.T) {
	t.Parallel()
	corpus, err := quality.LoadCorpus("../../../testdata/tts_quality/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	textSegmenter, err := segment.NewWithProfile(segment.ProsodyV2, 60)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range corpus.Cases {
		result := textSegmenter.SplitDetailed(testCase.SourceText)
		if len(result.Segments) == 0 {
			t.Errorf("case %s produced no segments", testCase.ID)
			continue
		}
		if compactWhitespace(strings.Join(result.Segments, "")) !=
			compactWhitespace(testCase.SourceText) {
			t.Errorf("case %s failed text conservation", testCase.ID)
		}
		for index, chunk := range result.Segments {
			if words := len(strings.Fields(chunk)); words > segment.OmniVoiceHardMaxWords {
				t.Errorf("case %s chunk %d words=%d", testCase.ID, index, words)
			}
			if runes := utf8.RuneCountInString(chunk); runes > segment.OmniVoiceHardMaxRunes {
				t.Errorf("case %s chunk %d runes=%d", testCase.ID, index, runes)
			}
		}
	}
}

func testConfig(output, experiment string, profile segment.Profile) Config {
	return Config{
		ExperimentID:            experiment,
		CorpusPath:              "../../../testdata/tts_quality/corpus.json",
		OutputRoot:              output,
		CaseIDs:                 []string{"chapter-transition-scene"},
		ReferenceAudio:          []byte("reference"),
		ReferenceContentType:    "audio/flac",
		ReferenceText:           "Текст референса.",
		ReferencePath:           "reference.flac",
		ReferenceTextPath:       "reference.txt",
		ReferenceTextFileSHA256: "test",
		Seed:                    42,
		SilenceThresholdDBFS:    -50,
		Settings:                DefaultGenerationSettings(),
		SegmenterProfile:        profile,
		SegmenterMaxWords:       60,
	}
}
