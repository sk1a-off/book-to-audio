// Command quality-runner generates controlled OmniVoice samples and provenance.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"book-text-editor/api"
	"book-text-editor/internal/quality"
	"book-text-editor/internal/qualityrun"
	"book-text-editor/internal/russiantext"
	"book-text-editor/internal/segment"
)

const (
	maxReferenceBytes     = 5 << 20
	maxTextOverridesBytes = 1 << 20
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		_, _ = fmt.Fprintf(os.Stderr, "quality runner failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	settings := qualityrun.DefaultGenerationSettings()
	flags := flag.NewFlagSet("quality-runner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	experimentID := flags.String("experiment", "", "unique experiment ID (required)")
	endpoint := flags.String("endpoint", "http://127.0.0.1:8001", "OmniVoice worker base URL")
	corpusPath := flags.String("corpus", "../testdata/tts_quality/corpus.json", "quality corpus path")
	outputRoot := flags.String("output", "../testdata/tts_quality/outputs", "experiment output root")
	referencePath := flags.String("reference", "../data/voice_reference_omnivoice.flac", "FLAC or WAV speaker reference")
	referenceTextPath := flags.String("reference-text", "../data/transcription.txt", "UTF-8 speaker reference transcript")
	caseList := flags.String("cases", strings.Join(qualityrun.PreviewCaseIDs(), ","), "comma-separated corpus case IDs")
	allCases := flags.Bool("all", false, "generate every corpus case instead of -cases")
	seed := flags.Uint64("seed", 42, "deterministic generation seed")
	timeout := flags.Duration("timeout", 10*time.Minute, "timeout per worker request")
	silenceThreshold := flags.Float64("silence-threshold-dbfs", -50, "silence threshold in negative dBFS")
	interChunkMinPauseMS := flags.Int(
		"inter-chunk-min-pause-ms",
		0,
		"experimental minimum silence at generated chunk boundaries (0 disables)",
	)
	boundaryMinPauses := flags.String(
		"boundary-min-pauses-ms",
		"",
		"experimental comma-separated minimum pauses for each generated boundary",
	)
	segmenterProfile := flags.String(
		"segmenter",
		string(segment.LegacyV1),
		"segmentation profile: legacy-v1 or prosody-v2",
	)
	segmenterMaxWords := flags.Int(
		"segmenter-max-words",
		segment.DefaultMaxWords,
		"preferred words per generated chunk",
	)
	textOverridesPath := flags.String(
		"text-overrides",
		"",
		"optional versioned source-to-TTS text overrides JSON",
	)
	allowDenseStressOverrides := flags.Bool(
		"allow-dense-stress-overrides",
		false,
		"experiment only: permit stress marks in half or more words",
	)
	selectiveStress := flags.Bool(
		"selective-stress",
		false,
		"experiment: apply only built-in phrase-scoped Russian stress rules",
	)
	normalizeRussianMorphology := flags.Bool(
		"normalize-russian-morphology",
		false,
		"experiment: inflect supported Russian numbers, dates, units, and headings",
	)
	flags.IntVar(&settings.NumSteps, "num-steps", settings.NumSteps, "OmniVoice diffusion steps")
	flags.Float64Var(&settings.GuidanceScale, "guidance", settings.GuidanceScale, "OmniVoice guidance scale")
	flags.Float64Var(&settings.Speed, "speed", settings.Speed, "speech speed")
	flags.BoolVar(&settings.NormalizeText, "normalize-text", settings.NormalizeText, "enable OmniVoice text normalization")
	flags.BoolVar(&settings.Denoise, "denoise", settings.Denoise, "enable reference denoise")
	flags.Float64Var(&settings.TShift, "t-shift", settings.TShift, "OmniVoice t_shift")
	flags.Float64Var(&settings.LayerPenaltyFactor, "layer-penalty", settings.LayerPenaltyFactor, "layer conditioning penalty")
	flags.Float64Var(&settings.PositionTemperature, "position-temperature", settings.PositionTemperature, "position temperature")
	flags.Float64Var(&settings.ClassTemperature, "class-temperature", settings.ClassTemperature, "class temperature")
	flags.BoolVar(&settings.PreprocessPrompt, "preprocess-prompt", settings.PreprocessPrompt, "preprocess voice prompt")
	flags.BoolVar(&settings.PostprocessOutput, "postprocess-output", settings.PostprocessOutput, "postprocess generated audio")
	flags.Float64Var(&settings.AudioChunkDuration, "audio-chunk-duration", settings.AudioChunkDuration, "audio processing chunk duration")
	flags.Float64Var(&settings.AudioChunkThreshold, "audio-chunk-threshold", settings.AudioChunkThreshold, "audio processing chunk threshold")
	flags.Float64Var(&settings.PadDuration, "pad-duration", settings.PadDuration, "boundary padding duration")
	flags.Float64Var(&settings.FadeDuration, "fade-duration", settings.FadeDuration, "boundary fade duration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if strings.TrimSpace(*experimentID) == "" {
		return errors.New("-experiment is required")
	}
	if *seed > uint64(^uint32(0)) {
		return errors.New("-seed exceeds uint32")
	}
	if *timeout <= 0 {
		return errors.New("-timeout must be positive")
	}
	profile := segment.Profile(strings.TrimSpace(*segmenterProfile))
	if profile != segment.LegacyV1 && profile != segment.ProsodyV2 {
		return fmt.Errorf("unsupported -segmenter value %q", *segmenterProfile)
	}
	if *segmenterMaxWords <= 0 {
		return errors.New("-segmenter-max-words must be positive")
	}
	if *interChunkMinPauseMS < 0 || *interChunkMinPauseMS > 5_000 {
		return errors.New("-inter-chunk-min-pause-ms must be between 0 and 5000")
	}
	boundaryMinPausesMS, err := parsePauseSequence(*boundaryMinPauses)
	if err != nil {
		return err
	}
	if *interChunkMinPauseMS > 0 && len(boundaryMinPausesMS) > 0 {
		return errors.New(
			"-inter-chunk-min-pause-ms and -boundary-min-pauses-ms are mutually exclusive",
		)
	}
	if strings.TrimSpace(*textOverridesPath) != "" &&
		(*selectiveStress || *normalizeRussianMorphology) {
		return errors.New(
			"-text-overrides cannot be combined with built-in Russian preparation flags",
		)
	}

	referenceAudio, err := readBoundedFile(*referencePath, maxReferenceBytes)
	if err != nil {
		return fmt.Errorf("read speaker reference: %w", err)
	}
	referenceTextBytes, err := readBoundedFile(*referenceTextPath, 4_000)
	if err != nil {
		return fmt.Errorf("read speaker reference transcript: %w", err)
	}
	referenceText := strings.TrimSpace(string(referenceTextBytes))
	caseIDs, err := resolveCaseIDs(*corpusPath, *caseList, *allCases)
	if err != nil {
		return err
	}
	contentType, err := referenceContentType(*referencePath)
	if err != nil {
		return err
	}
	var (
		textOverrides       *quality.TextOverrides
		textOverridesSHA256 string
	)
	if strings.TrimSpace(*textOverridesPath) != "" {
		overrideBytes, err := readBoundedFile(*textOverridesPath, maxTextOverridesBytes)
		if err != nil {
			return fmt.Errorf("read text overrides: %w", err)
		}
		decoded, err := quality.DecodeTextOverridesWithOptions(
			bytes.NewReader(overrideBytes),
			quality.TextOverrideValidationOptions{
				AllowDenseStress: *allowDenseStressOverrides,
			},
		)
		if err != nil {
			return fmt.Errorf("decode text overrides: %w", err)
		}
		textOverrides = &decoded
		textOverridesSHA256 = sha256Hex(overrideBytes)
	}

	client, err := api.NewOmniVoiceHTTPClient(*endpoint, &http.Client{Timeout: *timeout})
	if err != nil {
		return err
	}
	report, err := qualityrun.Run(ctx, client, qualityrun.Config{
		ExperimentID:              *experimentID,
		CorpusPath:                *corpusPath,
		OutputRoot:                *outputRoot,
		CaseIDs:                   caseIDs,
		ReferenceAudio:            referenceAudio,
		ReferenceContentType:      contentType,
		ReferenceText:             referenceText,
		ReferencePath:             *referencePath,
		ReferenceTextPath:         *referenceTextPath,
		ReferenceTextFileSHA256:   sha256Hex(referenceTextBytes),
		Seed:                      uint32(*seed),
		SilenceThresholdDBFS:      *silenceThreshold,
		InterChunkMinPauseMS:      *interChunkMinPauseMS,
		BoundaryMinPausesMS:       boundaryMinPausesMS,
		Settings:                  settings,
		SegmenterProfile:          profile,
		SegmenterMaxWords:         *segmenterMaxWords,
		TextOverrides:             textOverrides,
		TextOverridesPath:         strings.TrimSpace(*textOverridesPath),
		TextOverridesSHA256:       textOverridesSHA256,
		AllowDenseStressOverrides: *allowDenseStressOverrides,
		RussianTextOptions: russiantext.Options{
			SelectiveStress:     *selectiveStress,
			NormalizeMorphology: *normalizeRussianMorphology,
		},
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		stdout,
		"quality experiment complete: id=%s samples=%d audio=%.2fs wall=%.2fs output=%s\n",
		report.ExperimentID,
		report.SampleCount,
		report.AudioSeconds,
		report.WallSeconds,
		report.OutputDir,
	)
	return err
}

func parsePauseSequence(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	result := make([]int, 0, len(parts))
	for index, part := range parts {
		part = strings.TrimSpace(part)
		pauseMS, err := strconv.Atoi(part)
		if err != nil || pauseMS < 0 || pauseMS > 5_000 {
			return nil, fmt.Errorf(
				"-boundary-min-pauses-ms item %d must be an integer between 0 and 5000",
				index+1,
			)
		}
		result = append(result, pauseMS)
	}
	return result, nil
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func resolveCaseIDs(corpusPath, value string, all bool) ([]string, error) {
	if all {
		corpus, err := quality.LoadCorpus(corpusPath)
		if err != nil {
			return nil, err
		}
		result := make([]string, 0, len(corpus.Cases))
		for _, testCase := range corpus.Cases {
			result = append(result, testCase.ID)
		}
		return result, nil
	}
	var result []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("-cases must contain at least one case ID")
	}
	return result, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- this local CLI intentionally opens an operator-supplied path.
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return content, nil
}

func referenceContentType(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		return "audio/flac", nil
	case ".wav":
		return "audio/wav", nil
	default:
		return "", errors.New("speaker reference must have .flac or .wav extension")
	}
}
