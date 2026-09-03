package qualityrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"book-text-editor/api"
	"book-text-editor/internal/quality"
	"book-text-editor/internal/russiantext"
	"book-text-editor/internal/segment"
)

var experimentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

var defaultPreviewCases = []string{
	"prose-long-period",
	"dialogue-question-answer",
	"question-and-exclamation",
	"numbers-gender-and-ordinal",
	"homograph-zamok",
}

func PreviewCaseIDs() []string {
	return slices.Clone(defaultPreviewCases)
}

type Generator interface {
	Generate(context.Context, api.TTSRequest) (api.TTSResult, error)
}

type GenerationSettings struct {
	NumSteps            int     `json:"num_steps"`
	GuidanceScale       float64 `json:"guidance_scale"`
	Speed               float64 `json:"speed"`
	NormalizeText       bool    `json:"normalize_text"`
	Denoise             bool    `json:"denoise"`
	TShift              float64 `json:"t_shift"`
	LayerPenaltyFactor  float64 `json:"layer_penalty_factor"`
	PositionTemperature float64 `json:"position_temperature"`
	ClassTemperature    float64 `json:"class_temperature"`
	PreprocessPrompt    bool    `json:"preprocess_prompt"`
	PostprocessOutput   bool    `json:"postprocess_output"`
	AudioChunkDuration  float64 `json:"audio_chunk_duration"`
	AudioChunkThreshold float64 `json:"audio_chunk_threshold"`
	PadDuration         float64 `json:"pad_duration"`
	FadeDuration        float64 `json:"fade_duration"`
}

func DefaultGenerationSettings() GenerationSettings {
	return GenerationSettings{
		NumSteps:            32,
		GuidanceScale:       2,
		Speed:               1,
		NormalizeText:       true,
		Denoise:             true,
		TShift:              0.1,
		LayerPenaltyFactor:  5,
		PositionTemperature: 5,
		ClassTemperature:    0,
		PreprocessPrompt:    true,
		PostprocessOutput:   true,
		AudioChunkDuration:  15,
		AudioChunkThreshold: 30,
		PadDuration:         0.1,
		FadeDuration:        0.1,
	}
}

type Config struct {
	ExperimentID              string
	CorpusPath                string
	OutputRoot                string
	CaseIDs                   []string
	ReferenceAudio            []byte
	ReferenceContentType      string
	ReferenceText             string
	ReferencePath             string
	ReferenceTextPath         string
	ReferenceTextFileSHA256   string
	Seed                      uint32
	SilenceThresholdDBFS      float64
	InterChunkMinPauseMS      int
	BoundaryMinPausesMS       []int
	Settings                  GenerationSettings
	SegmenterProfile          segment.Profile
	SegmenterMaxWords         int
	TextOverrides             *quality.TextOverrides
	TextOverridesPath         string
	TextOverridesSHA256       string
	AllowDenseStressOverrides bool
	RussianTextOptions        russiantext.Options
}

type RunReport struct {
	ExperimentID string
	OutputDir    string
	SampleCount  int
	AudioSeconds float64
	WallSeconds  float64
}

type sampleManifest struct {
	SchemaVersion          string                 `json:"schema_version"`
	SampleID               string                 `json:"sample_id"`
	CaseID                 string                 `json:"case_id"`
	CorpusVersion          string                 `json:"corpus_version"`
	SourceText             string                 `json:"source_text"`
	TTSText                string                 `json:"tts_text"`
	NormalizerVersion      *string                `json:"normalizer_version"`
	SegmenterVersion       *string                `json:"segmenter_version"`
	Engine                 string                 `json:"engine"`
	Model                  string                 `json:"model"`
	ModelRevision          string                 `json:"model_revision"`
	Seed                   uint32                 `json:"seed"`
	InferenceConfig        GenerationSettings     `json:"inference_config"`
	AudioAssembly          audioAssemblyManifest  `json:"audio_assembly"`
	SpeakerReferenceSHA256 string                 `json:"speaker_reference_sha256"`
	Artifact               artifactManifest       `json:"artifact"`
	Timing                 timingManifest         `json:"timing"`
	WorkerResponse         workerResponseManifest `json:"worker_response"`
	Chunks                 []chunkManifest        `json:"chunks"`
}

type artifactManifest struct {
	RelativePath             string   `json:"relative_path"`
	SHA256                   string   `json:"sha256"`
	PCMSHA256                string   `json:"pcm_sha256"`
	WorkerReportedPCM_SHA256 *string  `json:"worker_reported_pcm_sha256"`
	Codec                    string   `json:"codec"`
	SampleRateHz             int      `json:"sample_rate_hz"`
	Channels                 int      `json:"channels"`
	DurationSeconds          float64  `json:"duration_seconds"`
	PeakDBFS                 *float64 `json:"peak_dbfs"`
	RMSDBFS                  *float64 `json:"rms_dbfs"`
	LUFSI                    *float64 `json:"lufs_i"`
	ClippingPercent          float64  `json:"clipping_percent"`
	SilenceRatio             float64  `json:"silence_ratio"`
	LeadingSilenceSeconds    float64  `json:"leading_silence_seconds"`
	TrailingSilenceSeconds   float64  `json:"trailing_silence_seconds"`
	ContainsNaNOrInf         bool     `json:"contains_nan_or_inf"`
}

type timingManifest struct {
	PreprocessingSeconds *float64 `json:"preprocessing_seconds"`
	TokenizationSeconds  *float64 `json:"tokenization_seconds"`
	InferenceSeconds     *float64 `json:"inference_seconds"`
	VocoderSeconds       *float64 `json:"vocoder_seconds"`
	AudioIOSeconds       *float64 `json:"audio_io_seconds"`
	GenerationSeconds    float64  `json:"generation_seconds"`
	AudioSeconds         float64  `json:"audio_seconds"`
	RTF                  float64  `json:"rtf"`
	PeakVRAMBytes        *int64   `json:"peak_vram_bytes"`
}

type workerResponseManifest struct {
	GenerationDurationMS int      `json:"generation_duration_ms"`
	AudioDurationMS      int      `json:"audio_duration_ms"`
	SeedUsed             uint32   `json:"seed_used"`
	VoiceCacheHit        *bool    `json:"voice_cache_hit"`
	Warnings             []string `json:"warnings"`
}

type chunkManifest struct {
	Index                    int                    `json:"index"`
	RequestID                string                 `json:"request_id"`
	Text                     string                 `json:"text"`
	Seed                     uint32                 `json:"seed"`
	PCMSHA256                string                 `json:"pcm_sha256"`
	WorkerReportedPCM_SHA256 string                 `json:"worker_reported_pcm_sha256"`
	AudioDurationMS          int                    `json:"audio_duration_ms"`
	GenerationDurationMS     int                    `json:"generation_duration_ms"`
	Warnings                 []string               `json:"warnings"`
	BoundaryBefore           *boundaryPauseManifest `json:"boundary_before,omitempty"`
}

type audioAssemblyManifest struct {
	Policy               string  `json:"policy"`
	InterChunkMinPauseMS int     `json:"inter_chunk_min_pause_ms"`
	BoundaryMinPausesMS  []int   `json:"boundary_min_pauses_ms,omitempty"`
	SilenceThresholdDBFS float64 `json:"silence_threshold_dbfs"`
}

type boundaryPauseManifest struct {
	TargetSeconds    float64 `json:"target_seconds"`
	ExistingSeconds  float64 `json:"existing_seconds"`
	AddedSeconds     float64 `json:"added_seconds"`
	ResultingSeconds float64 `json:"resulting_seconds"`
}

type experimentManifest struct {
	SchemaVersion             string                `json:"schema_version"`
	ExperimentID              string                `json:"experiment_id"`
	StartedAt                 time.Time             `json:"started_at"`
	CompletedAt               time.Time             `json:"completed_at"`
	CorpusVersion             string                `json:"corpus_version"`
	CorpusPath                string                `json:"corpus_path"`
	CaseIDs                   []string              `json:"case_ids"`
	ReferencePath             string                `json:"reference_path"`
	ReferenceTextPath         string                `json:"reference_text_path"`
	ReferenceSHA256           string                `json:"reference_sha256"`
	ReferenceTextSHA256       string                `json:"reference_text_sha256"`
	ReferenceTextFileSHA256   string                `json:"reference_text_file_sha256"`
	Seed                      uint32                `json:"seed"`
	SilenceThresholdDBFS      float64               `json:"silence_threshold_dbfs"`
	AudioAssembly             audioAssemblyManifest `json:"audio_assembly"`
	InferenceConfig           GenerationSettings    `json:"inference_config"`
	SegmenterVersion          string                `json:"segmenter_version"`
	SegmenterMaxWords         int                   `json:"segmenter_max_words"`
	NormalizerVersion         *string               `json:"normalizer_version"`
	TextOverridesPath         *string               `json:"text_overrides_path"`
	TextOverridesSHA256       *string               `json:"text_overrides_sha256"`
	AllowDenseStressOverrides bool                  `json:"allow_dense_stress_overrides"`
	RussianText               russiantext.Options   `json:"russian_text"`
	SampleManifests           []string              `json:"sample_manifests"`
	TotalAudioSeconds         float64               `json:"total_audio_seconds"`
	TotalWallSeconds          float64               `json:"total_wall_seconds"`
}

func Run(ctx context.Context, generator Generator, config Config) (RunReport, error) {
	if generator == nil {
		return RunReport{}, errors.New("generator is required")
	}
	if err := validateConfig(config); err != nil {
		return RunReport{}, err
	}
	corpus, err := quality.LoadCorpus(config.CorpusPath)
	if err != nil {
		return RunReport{}, err
	}
	selected, err := selectCases(corpus.Cases, config.CaseIDs)
	if err != nil {
		return RunReport{}, err
	}
	textSegmenter, err := segment.NewWithProfile(
		config.SegmenterProfile,
		config.SegmenterMaxWords,
	)
	if err != nil {
		return RunReport{}, fmt.Errorf("create segmenter: %w", err)
	}
	textOverrides, normalizerVersion, err := validatedTextOverrides(
		corpus,
		config.TextOverrides,
		config.AllowDenseStressOverrides,
	)
	if err != nil {
		return RunReport{}, err
	}
	if config.RussianTextOptions.SelectiveStress ||
		config.RussianTextOptions.NormalizeMorphology {
		version := russianTextNormalizerVersion(config.RussianTextOptions)
		normalizerVersion = &version
	}

	experimentDir := filepath.Join(config.OutputRoot, config.ExperimentID)
	if err := os.MkdirAll(config.OutputRoot, 0o750); err != nil {
		return RunReport{}, fmt.Errorf("create output root: %w", err)
	}
	if err := os.Mkdir(experimentDir, 0o750); err != nil {
		if errors.Is(err, os.ErrExist) {
			return RunReport{}, fmt.Errorf("experiment output %q already exists", experimentDir)
		}
		return RunReport{}, fmt.Errorf("create experiment output: %w", err)
	}

	startedAt := time.Now().UTC()
	referenceHash := sha256Hex(config.ReferenceAudio)
	referenceTextHash := sha256Hex([]byte(config.ReferenceText))
	manifestPaths := make([]string, 0, len(selected))
	var totalAudioSeconds float64

	for index, testCase := range selected {
		if err := ctx.Err(); err != nil {
			return RunReport{}, err
		}
		ttsText := testCase.SourceText
		if override, ok := textOverrides[testCase.ID]; ok {
			ttsText = override
		}
		if config.RussianTextOptions.SelectiveStress ||
			config.RussianTextOptions.NormalizeMorphology {
			ttsText, _ = russiantext.Prepare(ttsText, config.RussianTextOptions)
		}
		segmentation := textSegmenter.SplitDetailed(ttsText)
		if len(segmentation.Segments) == 0 {
			return RunReport{}, fmt.Errorf("segment case %q: no output", testCase.ID)
		}
		if compactWhitespace(strings.Join(segmentation.Segments, "")) !=
			compactWhitespace(ttsText) {
			return RunReport{}, fmt.Errorf("segment case %q: text conservation failed", testCase.ID)
		}
		requestStarted := time.Now()
		result, chunks, err := generateChunks(
			ctx,
			generator,
			config,
			index,
			testCase.ID,
			segmentation.Segments,
		)
		wallSeconds := time.Since(requestStarted).Seconds()
		if err != nil {
			return RunReport{}, fmt.Errorf("generate case %q: %w", testCase.ID, err)
		}
		if result.SampleWidth != 2 {
			return RunReport{}, fmt.Errorf(
				"generate case %q: sample width %d is not PCM16",
				testCase.ID,
				result.SampleWidth,
			)
		}
		if result.ModelID == "" || result.ModelVersion == "" || result.SeedUsed == nil {
			return RunReport{}, fmt.Errorf(
				"generate case %q: worker omitted reproducibility headers",
				testCase.ID,
			)
		}

		metrics, err := quality.AnalyzePCM16(
			result.AudioPCM,
			result.SampleRate,
			result.Channels,
			config.SilenceThresholdDBFS,
		)
		if err != nil {
			return RunReport{}, fmt.Errorf("analyze case %q: %w", testCase.ID, err)
		}
		wav, err := quality.EncodePCM16WAV(result.AudioPCM, result.SampleRate, result.Channels)
		if err != nil {
			return RunReport{}, fmt.Errorf("encode case %q: %w", testCase.ID, err)
		}
		wavName := testCase.ID + ".wav"
		if err := writeNewAtomic(filepath.Join(experimentDir, wavName), wav, 0o640); err != nil {
			return RunReport{}, fmt.Errorf("write case %q audio: %w", testCase.ID, err)
		}

		manifest := buildSampleManifest(
			config,
			corpus.CorpusVersion,
			testCase,
			result,
			metrics,
			wavName,
			sha256Hex(wav),
			sha256Hex(result.AudioPCM),
			wallSeconds,
			chunks,
			normalizerVersion,
		)
		manifestName := testCase.ID + ".json"
		if err := writeJSONNewAtomic(filepath.Join(experimentDir, manifestName), manifest); err != nil {
			return RunReport{}, fmt.Errorf("write case %q manifest: %w", testCase.ID, err)
		}
		manifestPaths = append(manifestPaths, manifestName)
		totalAudioSeconds += metrics.DurationSeconds
	}

	completedAt := time.Now().UTC()
	wallSeconds := completedAt.Sub(startedAt).Seconds()
	experiment := experimentManifest{
		SchemaVersion:             "tts-quality-experiment-v1",
		ExperimentID:              config.ExperimentID,
		StartedAt:                 startedAt,
		CompletedAt:               completedAt,
		CorpusVersion:             corpus.CorpusVersion,
		CorpusPath:                config.CorpusPath,
		CaseIDs:                   caseIDs(selected),
		ReferencePath:             config.ReferencePath,
		ReferenceTextPath:         config.ReferenceTextPath,
		ReferenceSHA256:           referenceHash,
		ReferenceTextSHA256:       referenceTextHash,
		ReferenceTextFileSHA256:   config.ReferenceTextFileSHA256,
		Seed:                      config.Seed,
		SilenceThresholdDBFS:      config.SilenceThresholdDBFS,
		AudioAssembly:             buildAudioAssemblyManifest(config),
		InferenceConfig:           config.Settings,
		SegmenterVersion:          string(config.SegmenterProfile),
		SegmenterMaxWords:         config.SegmenterMaxWords,
		NormalizerVersion:         normalizerVersion,
		TextOverridesPath:         optionalString(config.TextOverridesPath),
		TextOverridesSHA256:       optionalString(config.TextOverridesSHA256),
		AllowDenseStressOverrides: config.AllowDenseStressOverrides,
		RussianText:               config.RussianTextOptions,
		SampleManifests:           manifestPaths,
		TotalAudioSeconds:         totalAudioSeconds,
		TotalWallSeconds:          wallSeconds,
	}
	if err := writeJSONNewAtomic(filepath.Join(experimentDir, "experiment.json"), experiment); err != nil {
		return RunReport{}, fmt.Errorf("write experiment manifest: %w", err)
	}
	return RunReport{
		ExperimentID: config.ExperimentID,
		OutputDir:    experimentDir,
		SampleCount:  len(selected),
		AudioSeconds: totalAudioSeconds,
		WallSeconds:  wallSeconds,
	}, nil
}

func compactWhitespace(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsSpace(character) || character == '\u200b' || character == '\ufeff' {
			return -1
		}
		return character
	}, value)
}

func validateConfig(config Config) error {
	if !experimentIDPattern.MatchString(config.ExperimentID) {
		return fmt.Errorf("invalid experiment ID %q", config.ExperimentID)
	}
	if config.CorpusPath == "" || config.OutputRoot == "" {
		return errors.New("corpus path and output root are required")
	}
	if len(config.ReferenceAudio) == 0 || strings.TrimSpace(config.ReferenceText) == "" {
		return errors.New("reference audio and reference text are required")
	}
	if len(config.ReferenceText) > 4_000 {
		return errors.New("reference text exceeds the worker limit")
	}
	if config.ReferenceContentType != "audio/flac" && config.ReferenceContentType != "audio/wav" {
		return fmt.Errorf("unsupported reference content type %q", config.ReferenceContentType)
	}
	if len(config.CaseIDs) == 0 {
		return errors.New("at least one case ID is required")
	}
	if config.SilenceThresholdDBFS >= 0 {
		return errors.New("silence threshold must be negative")
	}
	if config.InterChunkMinPauseMS < 0 || config.InterChunkMinPauseMS > 5_000 {
		return errors.New("inter-chunk minimum pause must be between 0 and 5000 ms")
	}
	if config.InterChunkMinPauseMS > 0 && len(config.BoundaryMinPausesMS) > 0 {
		return errors.New("uniform and per-boundary minimum pauses are mutually exclusive")
	}
	for index, pauseMS := range config.BoundaryMinPausesMS {
		if pauseMS < 0 || pauseMS > 5_000 {
			return fmt.Errorf(
				"boundary minimum pause %d must be between 0 and 5000 ms",
				index+1,
			)
		}
	}
	if config.SegmenterProfile != segment.LegacyV1 &&
		config.SegmenterProfile != segment.ProsodyV2 {
		return fmt.Errorf("unsupported segmenter profile %q", config.SegmenterProfile)
	}
	if config.SegmenterMaxWords <= 0 {
		return errors.New("segmenter max words must be positive")
	}
	if config.TextOverrides == nil {
		if config.TextOverridesPath != "" || config.TextOverridesSHA256 != "" {
			return errors.New("text override provenance exists without overrides")
		}
	} else if config.TextOverridesPath == "" ||
		!sha256Pattern.MatchString(config.TextOverridesSHA256) {
		return errors.New("text overrides require path and SHA-256 provenance")
	}
	if config.TextOverrides != nil &&
		(config.RussianTextOptions.SelectiveStress ||
			config.RussianTextOptions.NormalizeMorphology) {
		return errors.New("text overrides and built-in Russian preparation are mutually exclusive")
	}
	return nil
}

func russianTextNormalizerVersion(options russiantext.Options) string {
	switch {
	case options.SelectiveStress && options.NormalizeMorphology:
		return russiantext.Version + ":stress+morphology"
	case options.SelectiveStress:
		return russiantext.Version + ":stress"
	case options.NormalizeMorphology:
		return russiantext.Version + ":morphology"
	default:
		return ""
	}
}

func validatedTextOverrides(
	corpus quality.Corpus,
	overrides *quality.TextOverrides,
	allowDenseStress bool,
) (map[string]string, *string, error) {
	if overrides == nil {
		return map[string]string{}, nil, nil
	}
	if err := overrides.ValidateWithOptions(quality.TextOverrideValidationOptions{
		AllowDenseStress: allowDenseStress,
	}); err != nil {
		return nil, nil, fmt.Errorf("validate text overrides: %w", err)
	}
	sources := make(map[string]string, len(corpus.Cases))
	for _, testCase := range corpus.Cases {
		sources[testCase.ID] = testCase.SourceText
	}
	result := overrides.ByCaseID()
	for caseID, ttsText := range result {
		source, exists := sources[caseID]
		if !exists {
			return nil, nil, fmt.Errorf("text override references unknown case %q", caseID)
		}
		if err := quality.ValidateStressOverride(source, ttsText); err != nil {
			return nil, nil, fmt.Errorf("text override %q: %w", caseID, err)
		}
	}
	version := overrides.NormalizerVersion
	return result, &version, nil
}

func selectCases(cases []quality.Case, selectedIDs []string) ([]quality.Case, error) {
	wanted := make(map[string]struct{}, len(selectedIDs))
	for _, id := range selectedIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errors.New("case IDs must not be empty")
		}
		if _, duplicate := wanted[id]; duplicate {
			return nil, fmt.Errorf("duplicate case ID %q", id)
		}
		wanted[id] = struct{}{}
	}
	selected := make([]quality.Case, 0, len(wanted))
	for _, testCase := range cases {
		if _, ok := wanted[testCase.ID]; ok {
			selected = append(selected, testCase)
			delete(wanted, testCase.ID)
		}
	}
	if len(wanted) > 0 {
		unknown := make([]string, 0, len(wanted))
		for id := range wanted {
			unknown = append(unknown, id)
		}
		slices.Sort(unknown)
		return nil, fmt.Errorf("unknown corpus case IDs: %s", strings.Join(unknown, ", "))
	}
	return selected, nil
}

func buildRequest(config Config, requestID, text string, seed uint32) api.TTSRequest {
	settings := config.Settings
	return api.TTSRequest{
		RequestID:            requestID,
		JobID:                config.ExperimentID,
		FragmentID:           requestID,
		Text:                 text,
		ReferenceAudio:       config.ReferenceAudio,
		ReferenceContentType: config.ReferenceContentType,
		ReferenceText:        config.ReferenceText,
		NumSteps:             settings.NumSteps,
		GuidanceScale:        settings.GuidanceScale,
		Speed:                settings.Speed,
		NormalizeText:        settings.NormalizeText,
		Denoise:              settings.Denoise,
		TShift:               settings.TShift,
		LayerPenaltyFactor:   settings.LayerPenaltyFactor,
		PositionTemperature:  settings.PositionTemperature,
		ClassTemperature:     settings.ClassTemperature,
		PreprocessPrompt:     settings.PreprocessPrompt,
		PostprocessOutput:    settings.PostprocessOutput,
		AudioChunkDuration:   settings.AudioChunkDuration,
		AudioChunkThreshold:  settings.AudioChunkThreshold,
		PadDuration:          settings.PadDuration,
		FadeDuration:         settings.FadeDuration,
		Seed:                 &seed,
		SettingsResolved:     true,
	}
}

func generateChunks(
	ctx context.Context,
	generator Generator,
	config Config,
	caseIndex int,
	caseID string,
	texts []string,
) (api.TTSResult, []chunkManifest, error) {
	if len(config.BoundaryMinPausesMS) > 0 &&
		len(config.BoundaryMinPausesMS) != len(texts)-1 {
		return api.TTSResult{}, nil, fmt.Errorf(
			"per-boundary pause count %d does not match %d generated boundaries",
			len(config.BoundaryMinPausesMS),
			len(texts)-1,
		)
	}
	var aggregate api.TTSResult
	chunks := make([]chunkManifest, 0, len(texts))
	allCacheHits := true
	cacheKnown := true
	for chunkIndex, text := range texts {
		seed64 := uint64(config.Seed) + uint64(chunkIndex)
		if seed64 > uint64(^uint32(0)) {
			return api.TTSResult{}, nil, errors.New("derived chunk seed exceeds uint32")
		}
		seed := uint32(seed64)
		requestID := fmt.Sprintf(
			"quality-%03d-%s-%03d",
			caseIndex+1,
			caseID,
			chunkIndex+1,
		)
		result, err := generator.Generate(ctx, buildRequest(config, requestID, text, seed))
		if err != nil {
			return api.TTSResult{}, nil, fmt.Errorf("chunk %d: %w", chunkIndex+1, err)
		}
		pauseMS := config.InterChunkMinPauseMS
		if chunkIndex > 0 && len(config.BoundaryMinPausesMS) > 0 {
			pauseMS = config.BoundaryMinPausesMS[chunkIndex-1]
		}
		boundary, err := appendChunkResult(&aggregate, result, config, pauseMS)
		if err != nil {
			return api.TTSResult{}, nil, fmt.Errorf("chunk %d: %w", chunkIndex+1, err)
		}
		if result.VoiceCacheHit == nil {
			cacheKnown = false
		} else if !*result.VoiceCacheHit {
			allCacheHits = false
		}
		chunks = append(chunks, chunkManifest{
			Index:                    chunkIndex,
			RequestID:                requestID,
			Text:                     text,
			Seed:                     seed,
			PCMSHA256:                sha256Hex(result.AudioPCM),
			WorkerReportedPCM_SHA256: result.AudioSHA256,
			AudioDurationMS:          result.DurationMS,
			GenerationDurationMS:     result.GenerationDurationMS,
			Warnings:                 append([]string{}, result.Warnings...),
			BoundaryBefore:           boundary,
		})
	}
	aggregateSeed := config.Seed
	aggregate.SeedUsed = &aggregateSeed
	if cacheKnown {
		aggregate.VoiceCacheHit = &allCacheHits
	}
	if len(chunks) == 1 {
		aggregate.AudioSHA256 = chunks[0].WorkerReportedPCM_SHA256
	}
	return aggregate, chunks, nil
}

func appendChunkResult(
	aggregate *api.TTSResult,
	result api.TTSResult,
	config Config,
	pauseMS int,
) (*boundaryPauseManifest, error) {
	if result.SampleWidth != 2 || len(result.AudioPCM) == 0 ||
		result.ModelID == "" || result.ModelVersion == "" ||
		result.SeedUsed == nil || result.AudioSHA256 == "" {
		return nil, errors.New("worker omitted audio or reproducibility metadata")
	}
	if len(aggregate.AudioPCM) == 0 {
		aggregate.RequestID = result.RequestID
		aggregate.SampleRate = result.SampleRate
		aggregate.Channels = result.Channels
		aggregate.SampleWidth = result.SampleWidth
		aggregate.ModelID = result.ModelID
		aggregate.ModelVersion = result.ModelVersion
	} else if aggregate.SampleRate != result.SampleRate ||
		aggregate.Channels != result.Channels ||
		aggregate.SampleWidth != result.SampleWidth ||
		aggregate.ModelID != result.ModelID ||
		aggregate.ModelVersion != result.ModelVersion {
		return nil, errors.New("worker changed audio format or model between chunks")
	}
	var boundary *boundaryPauseManifest
	if len(aggregate.AudioPCM) == 0 {
		aggregate.AudioPCM = append([]byte{}, result.AudioPCM...)
	} else if pauseMS == 0 {
		aggregate.AudioPCM = append(aggregate.AudioPCM, result.AudioPCM...)
	} else {
		joined, metrics, err := quality.AppendPCM16WithMinimumPause(
			aggregate.AudioPCM,
			result.AudioPCM,
			result.SampleRate,
			result.Channels,
			time.Duration(pauseMS)*time.Millisecond,
			config.SilenceThresholdDBFS,
		)
		if err != nil {
			return nil, fmt.Errorf("assemble adaptive boundary: %w", err)
		}
		boundary = &boundaryPauseManifest{
			TargetSeconds:    metrics.TargetSeconds,
			ExistingSeconds:  metrics.ExistingSeconds,
			AddedSeconds:     metrics.AddedSeconds,
			ResultingSeconds: metrics.ResultingSeconds,
		}
		aggregate.AudioPCM = joined
	}
	aggregate.DurationMS += result.DurationMS
	aggregate.GenerationDurationMS += result.GenerationDurationMS
	aggregate.Warnings = append(aggregate.Warnings, result.Warnings...)
	return boundary, nil
}

func buildSampleManifest(
	config Config,
	corpusVersion string,
	testCase quality.Case,
	result api.TTSResult,
	metrics quality.AudioMetrics,
	wavName string,
	wavHash string,
	pcmHash string,
	wallSeconds float64,
	chunks []chunkManifest,
	normalizerVersion *string,
) sampleManifest {
	workerInferenceSeconds := float64(result.GenerationDurationMS) / 1_000
	segmenterVersion := string(config.SegmenterProfile)
	workerPCMHash := optionalString(result.AudioSHA256)
	ttsChunks := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		ttsChunks = append(ttsChunks, chunk.Text)
	}
	return sampleManifest{
		SchemaVersion:          "tts-quality-sample-v1",
		SampleID:               config.ExperimentID + ":" + testCase.ID,
		CaseID:                 testCase.ID,
		CorpusVersion:          corpusVersion,
		SourceText:             testCase.SourceText,
		TTSText:                strings.Join(ttsChunks, "\n"),
		NormalizerVersion:      normalizerVersion,
		SegmenterVersion:       &segmenterVersion,
		Engine:                 "OmniVoice",
		Model:                  result.ModelID,
		ModelRevision:          result.ModelVersion,
		Seed:                   *result.SeedUsed,
		InferenceConfig:        config.Settings,
		AudioAssembly:          buildAudioAssemblyManifest(config),
		SpeakerReferenceSHA256: sha256Hex(config.ReferenceAudio),
		Artifact: artifactManifest{
			RelativePath:             wavName,
			SHA256:                   wavHash,
			PCMSHA256:                pcmHash,
			WorkerReportedPCM_SHA256: workerPCMHash,
			Codec:                    "pcm_s16le_wav",
			SampleRateHz:             result.SampleRate,
			Channels:                 result.Channels,
			DurationSeconds:          metrics.DurationSeconds,
			PeakDBFS:                 metrics.PeakDBFS,
			RMSDBFS:                  metrics.RMSDBFS,
			LUFSI:                    metrics.LUFSI,
			ClippingPercent:          metrics.ClippingPercent,
			SilenceRatio:             metrics.SilenceRatio,
			LeadingSilenceSeconds:    metrics.LeadingSilenceSeconds,
			TrailingSilenceSeconds:   metrics.TrailingSilenceSeconds,
			ContainsNaNOrInf:         metrics.ContainsNaNOrInf,
		},
		Timing: timingManifest{
			InferenceSeconds:  &workerInferenceSeconds,
			GenerationSeconds: wallSeconds,
			AudioSeconds:      metrics.DurationSeconds,
			RTF:               wallSeconds / metrics.DurationSeconds,
		},
		WorkerResponse: workerResponseManifest{
			GenerationDurationMS: result.GenerationDurationMS,
			AudioDurationMS:      result.DurationMS,
			SeedUsed:             *result.SeedUsed,
			VoiceCacheHit:        result.VoiceCacheHit,
			Warnings:             append([]string{}, result.Warnings...),
		},
		Chunks: chunks,
	}
}

func buildAudioAssemblyManifest(config Config) audioAssemblyManifest {
	policy := "disabled"
	if len(config.BoundaryMinPausesMS) > 0 {
		policy = "minimum-existing-silence-aware-explicit-boundaries-v1"
	} else if config.InterChunkMinPauseMS > 0 {
		policy = "minimum-existing-silence-aware-v1"
	}
	return audioAssemblyManifest{
		Policy:               policy,
		InterChunkMinPauseMS: config.InterChunkMinPauseMS,
		BoundaryMinPausesMS:  slices.Clone(config.BoundaryMinPausesMS),
		SilenceThresholdDBFS: config.SilenceThresholdDBFS,
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func writeJSONNewAtomic(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeNewAtomic(path, append(encoded, '\n'), 0o640)
}

func writeNewAtomic(path string, content []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".quality-tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func caseIDs(cases []quality.Case) []string {
	result := make([]string, 0, len(cases))
	for _, testCase := range cases {
		result = append(result, testCase.ID)
	}
	return result
}
