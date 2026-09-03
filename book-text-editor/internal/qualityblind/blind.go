// Package qualityblind builds reveal-safe listening packages from two
// controlled quality-runner experiments.
package qualityblind

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const (
	SessionSchemaVersion = "tts-quality-blind-session-v1"
	RevealSchemaVersion  = "tts-quality-blind-reveal-v1"
	SegmenterVariable    = "segmenter_version"
	FadeDurationVariable = "inference_config.fade_duration"
	EngineVariable       = "engine"
	NormalizerVariable   = "normalizer_version"
)

var (
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	caseIDPattern    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

type Config struct {
	SessionID       string
	ExperimentA     string
	ExperimentB     string
	OutputRoot      string
	ShuffleSeed     uint64
	CreatedAt       time.Time
	PrimaryVariable string
}

type Report struct {
	SessionID string
	OutputDir string
	CaseCount int
}

type experimentManifest struct {
	SchemaVersion           string          `json:"schema_version"`
	ExperimentID            string          `json:"experiment_id"`
	CorpusVersion           string          `json:"corpus_version"`
	CaseIDs                 []string        `json:"case_ids"`
	ReferenceSHA256         string          `json:"reference_sha256"`
	ReferenceTextSHA256     string          `json:"reference_text_sha256"`
	ReferenceTextFileSHA256 string          `json:"reference_text_file_sha256"`
	Seed                    uint32          `json:"seed"`
	SilenceThresholdDBFS    float64         `json:"silence_threshold_dbfs"`
	InferenceConfig         json.RawMessage `json:"inference_config"`
	SegmenterVersion        string          `json:"segmenter_version"`
	SegmenterMaxWords       int             `json:"segmenter_max_words"`
	NormalizerVersion       *string         `json:"normalizer_version"`
	TextOverridesSHA256     *string         `json:"text_overrides_sha256"`
}

type sampleManifest struct {
	SchemaVersion          string          `json:"schema_version"`
	SampleID               string          `json:"sample_id"`
	CaseID                 string          `json:"case_id"`
	CorpusVersion          string          `json:"corpus_version"`
	SourceText             string          `json:"source_text"`
	TTSText                string          `json:"tts_text"`
	NormalizerVersion      *string         `json:"normalizer_version"`
	SegmenterVersion       *string         `json:"segmenter_version"`
	Engine                 string          `json:"engine"`
	Model                  string          `json:"model"`
	ModelRevision          string          `json:"model_revision"`
	Seed                   uint32          `json:"seed"`
	InferenceConfig        json.RawMessage `json:"inference_config"`
	SpeakerReferenceSHA256 string          `json:"speaker_reference_sha256"`
	Artifact               struct {
		RelativePath    string  `json:"relative_path"`
		SHA256          string  `json:"sha256"`
		DurationSeconds float64 `json:"duration_seconds"`
	} `json:"artifact"`
	Chunks []struct {
		Text string `json:"text"`
	} `json:"chunks"`
}

type blindSession struct {
	SchemaVersion string      `json:"schema_version"`
	SessionID     string      `json:"session_id"`
	Method        string      `json:"method"`
	Cases         []blindCase `json:"cases"`
}

type blindCase struct {
	CaseID     string        `json:"case_id"`
	SourceText string        `json:"source_text"`
	Samples    []blindSample `json:"samples"`
}

type blindSample struct {
	BlindLabel      string  `json:"blind_label"`
	RelativePath    string  `json:"relative_path"`
	DurationSeconds float64 `json:"duration_seconds"`
}

type revealManifest struct {
	SchemaVersion   string       `json:"schema_version"`
	SessionID       string       `json:"session_id"`
	CreatedAt       time.Time    `json:"created_at"`
	PrimaryVariable string       `json:"primary_variable"`
	ShuffleSeed     uint64       `json:"shuffle_seed"`
	Cases           []revealCase `json:"cases"`
}

type revealCase struct {
	CaseID  string                  `json:"case_id"`
	Mapping map[string]revealSample `json:"mapping"`
}

type revealSample struct {
	ExperimentID      string          `json:"experiment_id"`
	SampleID          string          `json:"sample_id"`
	Engine            string          `json:"engine"`
	Model             string          `json:"model"`
	ModelRevision     string          `json:"model_revision"`
	SegmenterVersion  string          `json:"segmenter_version"`
	NormalizerVersion *string         `json:"normalizer_version"`
	TTSText           string          `json:"tts_text"`
	Chunks            []string        `json:"chunks"`
	ArtifactSHA256    string          `json:"artifact_sha256"`
	InferenceConfig   json.RawMessage `json:"inference_config"`
}

func Build(config Config) (Report, error) {
	if config.PrimaryVariable == "" {
		config.PrimaryVariable = SegmenterVariable
	}
	if err := validateConfig(config); err != nil {
		return Report{}, err
	}
	leftExperiment, err := readExperiment(config.ExperimentA)
	if err != nil {
		return Report{}, fmt.Errorf("read experiment A: %w", err)
	}
	rightExperiment, err := readExperiment(config.ExperimentB)
	if err != nil {
		return Report{}, fmt.Errorf("read experiment B: %w", err)
	}
	if err := validateControlledPair(
		leftExperiment,
		rightExperiment,
		config.PrimaryVariable,
	); err != nil {
		return Report{}, err
	}

	leftSamples, err := readSamples(config.ExperimentA, leftExperiment.CaseIDs)
	if err != nil {
		return Report{}, fmt.Errorf("read experiment A samples: %w", err)
	}
	rightSamples, err := readSamples(config.ExperimentB, rightExperiment.CaseIDs)
	if err != nil {
		return Report{}, fmt.Errorf("read experiment B samples: %w", err)
	}

	if err := os.MkdirAll(config.OutputRoot, 0o750); err != nil {
		return Report{}, fmt.Errorf("create output root: %w", err)
	}
	finalDir := filepath.Join(config.OutputRoot, config.SessionID)
	if _, err := os.Stat(finalDir); err == nil {
		return Report{}, fmt.Errorf("blind session output %q already exists", finalDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Report{}, fmt.Errorf("inspect output: %w", err)
	}
	temporaryDir, err := os.MkdirTemp(config.OutputRoot, "."+config.SessionID+".tmp-")
	if err != nil {
		return Report{}, fmt.Errorf("create temporary output: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(temporaryDir)
		}
	}()

	createdAt := config.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	random := rand.New(rand.NewPCG(config.ShuffleSeed, config.ShuffleSeed^0x9e3779b97f4a7c15))
	session := blindSession{
		SchemaVersion: SessionSchemaVersion,
		SessionID:     config.SessionID,
		Method:        "single-listener blind A/B preference; not MOS",
		Cases:         make([]blindCase, 0, len(leftExperiment.CaseIDs)),
	}
	reveal := revealManifest{
		SchemaVersion:   RevealSchemaVersion,
		SessionID:       config.SessionID,
		CreatedAt:       createdAt,
		PrimaryVariable: config.PrimaryVariable,
		ShuffleSeed:     config.ShuffleSeed,
		Cases:           make([]revealCase, 0, len(leftExperiment.CaseIDs)),
	}

	for _, caseID := range leftExperiment.CaseIDs {
		left := leftSamples[caseID]
		right := rightSamples[caseID]
		if err := validateSamplePair(left, right, config.PrimaryVariable); err != nil {
			return Report{}, fmt.Errorf("case %q: %w", caseID, err)
		}
		ordered := []struct {
			directory  string
			experiment experimentManifest
			sample     sampleManifest
		}{
			{config.ExperimentA, leftExperiment, left},
			{config.ExperimentB, rightExperiment, right},
		}
		if random.IntN(2) == 1 {
			ordered[0], ordered[1] = ordered[1], ordered[0]
		}

		blind := blindCase{CaseID: caseID, SourceText: left.SourceText}
		revealEntry := revealCase{CaseID: caseID, Mapping: make(map[string]revealSample, 2)}
		for index, item := range ordered {
			label := string(rune('A' + index))
			relativePath := filepath.ToSlash(filepath.Join("samples", caseID, label+".wav"))
			sourcePath, err := artifactPath(item.directory, item.sample.Artifact.RelativePath)
			if err != nil {
				return Report{}, fmt.Errorf("case %q label %s: %w", caseID, label, err)
			}
			if err := copyVerified(
				sourcePath,
				filepath.Join(temporaryDir, filepath.FromSlash(relativePath)),
				item.sample.Artifact.SHA256,
			); err != nil {
				return Report{}, fmt.Errorf("case %q label %s: %w", caseID, label, err)
			}
			blind.Samples = append(blind.Samples, blindSample{
				BlindLabel:      label,
				RelativePath:    relativePath,
				DurationSeconds: item.sample.Artifact.DurationSeconds,
			})
			chunks := make([]string, 0, len(item.sample.Chunks))
			for _, chunk := range item.sample.Chunks {
				chunks = append(chunks, chunk.Text)
			}
			revealEntry.Mapping[label] = revealSample{
				ExperimentID:      item.experiment.ExperimentID,
				SampleID:          item.sample.SampleID,
				Engine:            item.sample.Engine,
				Model:             item.sample.Model,
				ModelRevision:     item.sample.ModelRevision,
				SegmenterVersion:  item.experiment.SegmenterVersion,
				NormalizerVersion: item.sample.NormalizerVersion,
				TTSText:           item.sample.TTSText,
				Chunks:            chunks,
				ArtifactSHA256:    item.sample.Artifact.SHA256,
				InferenceConfig:   item.sample.InferenceConfig,
			}
		}
		session.Cases = append(session.Cases, blind)
		reveal.Cases = append(reveal.Cases, revealEntry)
	}

	if err := writeJSON(filepath.Join(temporaryDir, "session.json"), session); err != nil {
		return Report{}, err
	}
	if err := writeJSON(filepath.Join(temporaryDir, "reveal.json"), reveal); err != nil {
		return Report{}, err
	}
	if err := writeListeningPage(filepath.Join(temporaryDir, "index.html"), session); err != nil {
		return Report{}, err
	}
	if err := os.Rename(temporaryDir, finalDir); err != nil {
		return Report{}, fmt.Errorf("publish blind session: %w", err)
	}
	cleanup = false
	return Report{SessionID: config.SessionID, OutputDir: finalDir, CaseCount: len(session.Cases)}, nil
}

func validateConfig(config Config) error {
	if !sessionIDPattern.MatchString(config.SessionID) {
		return fmt.Errorf("invalid session ID %q", config.SessionID)
	}
	if strings.TrimSpace(config.ExperimentA) == "" || strings.TrimSpace(config.ExperimentB) == "" {
		return errors.New("both experiment directories are required")
	}
	if filepath.Clean(config.ExperimentA) == filepath.Clean(config.ExperimentB) {
		return errors.New("experiment directories must be different")
	}
	if strings.TrimSpace(config.OutputRoot) == "" {
		return errors.New("output root is required")
	}
	if config.PrimaryVariable != SegmenterVariable &&
		config.PrimaryVariable != FadeDurationVariable &&
		config.PrimaryVariable != EngineVariable &&
		config.PrimaryVariable != NormalizerVariable {
		return fmt.Errorf("unsupported primary variable %q", config.PrimaryVariable)
	}
	return nil
}

func readExperiment(directory string) (experimentManifest, error) {
	var manifest experimentManifest
	if err := readJSON(filepath.Join(directory, "experiment.json"), &manifest); err != nil {
		return manifest, err
	}
	if manifest.SchemaVersion != "tts-quality-experiment-v1" || manifest.ExperimentID == "" ||
		len(manifest.CaseIDs) == 0 {
		return manifest, errors.New("invalid experiment manifest")
	}
	return manifest, nil
}

func readSamples(directory string, caseIDs []string) (map[string]sampleManifest, error) {
	result := make(map[string]sampleManifest, len(caseIDs))
	for _, caseID := range caseIDs {
		if !caseIDPattern.MatchString(caseID) {
			return nil, fmt.Errorf("invalid case ID %q", caseID)
		}
		var sample sampleManifest
		if err := readJSON(filepath.Join(directory, caseID+".json"), &sample); err != nil {
			return nil, err
		}
		if sample.SchemaVersion != "tts-quality-sample-v1" || sample.CaseID != caseID ||
			sample.SampleID == "" || sample.SourceText == "" || sample.Artifact.SHA256 == "" {
			return nil, fmt.Errorf("invalid sample manifest for %q", caseID)
		}
		result[caseID] = sample
	}
	return result, nil
}

func validateControlledPair(
	left experimentManifest,
	right experimentManifest,
	primaryVariable string,
) error {
	if left.ExperimentID == right.ExperimentID {
		return errors.New("experiment IDs must be different")
	}
	checks := []struct {
		name  string
		left  any
		right any
	}{
		{"corpus version", left.CorpusVersion, right.CorpusVersion},
		{"case IDs", left.CaseIDs, right.CaseIDs},
		{"reference hash", left.ReferenceSHA256, right.ReferenceSHA256},
		{"reference text hash", left.ReferenceTextSHA256, right.ReferenceTextSHA256},
		{"reference text file hash", left.ReferenceTextFileSHA256, right.ReferenceTextFileSHA256},
		{"generation seed", left.Seed, right.Seed},
		{"silence threshold", left.SilenceThresholdDBFS, right.SilenceThresholdDBFS},
		{"segmenter max words", left.SegmenterMaxWords, right.SegmenterMaxWords},
	}
	for _, check := range checks {
		if !reflect.DeepEqual(check.left, check.right) {
			return fmt.Errorf("controlled comparison mismatch: %s", check.name)
		}
	}
	switch primaryVariable {
	case SegmenterVariable:
		if !reflect.DeepEqual(left.NormalizerVersion, right.NormalizerVersion) ||
			!reflect.DeepEqual(left.TextOverridesSHA256, right.TextOverridesSHA256) {
			return errors.New("controlled comparison mismatch: normalizer")
		}
		if left.SegmenterVersion == right.SegmenterVersion {
			return errors.New("segmenter versions must be different")
		}
		equal, err := equalJSON(left.InferenceConfig, right.InferenceConfig)
		if err != nil {
			return fmt.Errorf("compare inference config: %w", err)
		}
		if !equal {
			return errors.New("controlled comparison mismatch: inference config")
		}
	case FadeDurationVariable:
		if !reflect.DeepEqual(left.NormalizerVersion, right.NormalizerVersion) ||
			!reflect.DeepEqual(left.TextOverridesSHA256, right.TextOverridesSHA256) {
			return errors.New("controlled comparison mismatch: normalizer")
		}
		if left.SegmenterVersion != right.SegmenterVersion {
			return errors.New("controlled comparison mismatch: segmenter version")
		}
		if err := validateSingleInferenceDifference(
			left.InferenceConfig,
			right.InferenceConfig,
			"fade_duration",
		); err != nil {
			return fmt.Errorf("controlled comparison mismatch: %w", err)
		}
	case EngineVariable:
		if !reflect.DeepEqual(left.NormalizerVersion, right.NormalizerVersion) ||
			!reflect.DeepEqual(left.TextOverridesSHA256, right.TextOverridesSHA256) {
			return errors.New("controlled comparison mismatch: normalizer")
		}
		if left.SegmenterVersion != right.SegmenterVersion {
			return errors.New("controlled comparison mismatch: segmenter version")
		}
	case NormalizerVariable:
		if reflect.DeepEqual(left.NormalizerVersion, right.NormalizerVersion) {
			return errors.New("normalizer versions must be different")
		}
		if left.SegmenterVersion != right.SegmenterVersion {
			return errors.New("controlled comparison mismatch: segmenter version")
		}
		equal, err := equalJSON(left.InferenceConfig, right.InferenceConfig)
		if err != nil {
			return fmt.Errorf("compare inference config: %w", err)
		}
		if !equal {
			return errors.New("controlled comparison mismatch: inference config")
		}
	}
	return nil
}

func validateSamplePair(
	left sampleManifest,
	right sampleManifest,
	primaryVariable string,
) error {
	checks := []struct {
		name  string
		left  any
		right any
	}{
		{"case ID", left.CaseID, right.CaseID},
		{"corpus version", left.CorpusVersion, right.CorpusVersion},
		{"source text", left.SourceText, right.SourceText},
		{"generation seed", left.Seed, right.Seed},
		{"speaker reference hash", left.SpeakerReferenceSHA256, right.SpeakerReferenceSHA256},
	}
	for _, check := range checks {
		if !reflect.DeepEqual(check.left, check.right) {
			return fmt.Errorf("sample mismatch: %s", check.name)
		}
	}
	switch primaryVariable {
	case SegmenterVariable:
		if !reflect.DeepEqual(left.NormalizerVersion, right.NormalizerVersion) {
			return errors.New("sample mismatch: normalizer version")
		}
		equal, err := equalJSON(left.InferenceConfig, right.InferenceConfig)
		if err != nil {
			return err
		}
		if !equal {
			return errors.New("sample mismatch: inference config")
		}
	case FadeDurationVariable:
		if !reflect.DeepEqual(left.NormalizerVersion, right.NormalizerVersion) {
			return errors.New("sample mismatch: normalizer version")
		}
		if left.TTSText != right.TTSText {
			return errors.New("sample mismatch: TTS text")
		}
		if !reflect.DeepEqual(left.SegmenterVersion, right.SegmenterVersion) ||
			!reflect.DeepEqual(left.Chunks, right.Chunks) {
			return errors.New("sample mismatch: segmentation")
		}
		if err := validateSingleInferenceDifference(
			left.InferenceConfig,
			right.InferenceConfig,
			"fade_duration",
		); err != nil {
			return fmt.Errorf("sample mismatch: %w", err)
		}
	case EngineVariable:
		if !reflect.DeepEqual(left.NormalizerVersion, right.NormalizerVersion) {
			return errors.New("sample mismatch: normalizer version")
		}
		if left.TTSText != right.TTSText {
			return errors.New("sample mismatch: TTS text")
		}
		if !reflect.DeepEqual(left.SegmenterVersion, right.SegmenterVersion) ||
			!reflect.DeepEqual(left.Chunks, right.Chunks) {
			return errors.New("sample mismatch: segmentation")
		}
		if left.Engine == right.Engine && left.Model == right.Model &&
			left.ModelRevision == right.ModelRevision {
			return errors.New("engine comparison requires different engine or model identity")
		}
	case NormalizerVariable:
		if reflect.DeepEqual(left.NormalizerVersion, right.NormalizerVersion) {
			return errors.New("normalizer comparison requires different versions")
		}
		if !reflect.DeepEqual(left.SegmenterVersion, right.SegmenterVersion) {
			return errors.New("sample mismatch: segmenter version")
		}
		equal, err := equalJSON(left.InferenceConfig, right.InferenceConfig)
		if err != nil {
			return err
		}
		if !equal {
			return errors.New("sample mismatch: inference config")
		}
		if left.Engine != right.Engine || left.Model != right.Model ||
			left.ModelRevision != right.ModelRevision {
			return errors.New("sample mismatch: engine or model identity")
		}
	}
	return nil
}

func validateSingleInferenceDifference(
	left json.RawMessage,
	right json.RawMessage,
	field string,
) error {
	var leftConfig, rightConfig map[string]any
	if err := json.Unmarshal(left, &leftConfig); err != nil {
		return fmt.Errorf("decode left inference config: %w", err)
	}
	if err := json.Unmarshal(right, &rightConfig); err != nil {
		return fmt.Errorf("decode right inference config: %w", err)
	}
	leftValue, leftExists := leftConfig[field]
	rightValue, rightExists := rightConfig[field]
	if !leftExists || !rightExists {
		return fmt.Errorf("inference config omits %s", field)
	}
	if reflect.DeepEqual(leftValue, rightValue) {
		return fmt.Errorf("primary inference field %s must be different", field)
	}
	delete(leftConfig, field)
	delete(rightConfig, field)
	if !reflect.DeepEqual(leftConfig, rightConfig) {
		return errors.New("inference config differs outside the primary field")
	}
	return nil
}

func readJSON(path string, destination any) error {
	file, err := os.Open(path) // #nosec G304 -- local operator-supplied experiment path.
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decodeErr := decoder.Decode(destination)
	var extra any
	trailingErr := decoder.Decode(&extra)
	closeErr := file.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if trailingErr != io.EOF {
		return fmt.Errorf("trailing JSON: %v", trailingErr)
	}
	return closeErr
}

func equalJSON(left, right json.RawMessage) (bool, error) {
	var leftValue, rightValue any
	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false, err
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false, err
	}
	return reflect.DeepEqual(leftValue, rightValue), nil
}

func artifactPath(directory, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Base(relative) != relative {
		return "", fmt.Errorf("unsafe artifact path %q", relative)
	}
	return filepath.Join(directory, relative), nil
}

func copyVerified(sourcePath, destinationPath, wantHash string) error {
	if len(wantHash) != 64 {
		return errors.New("invalid artifact SHA-256")
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
		return err
	}
	source, err := os.Open(sourcePath) // #nosec G304 -- validated local artifact path.
	if err != nil {
		return err
	}
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		_ = source.Close()
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, 256<<20))
	closeErr := errors.Join(destination.Close(), source.Close())
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != wantHash {
		return fmt.Errorf("artifact SHA-256 %s, want %s", got, wantHash)
	}
	return nil
}

func writeJSON(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeListeningPage(path string, session blindSession) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	executeErr := listeningPageTemplate.Execute(file, session)
	closeErr := file.Close()
	return errors.Join(executeErr, closeErr)
}

var listeningPageTemplate = template.Must(template.New("blind-listening").Funcs(template.FuncMap{
	"add":     func(value, increment int) int { return value + increment },
	"seconds": func(value float64) string { return fmt.Sprintf("%.2f сек.", value) },
}).Parse(`<!doctype html>
<html lang="ru">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Слепое сравнение TTS</title>
  <style>
    :root { color-scheme: dark; font-family: Inter, system-ui, sans-serif; background: #101318; color: #edf2f7; }
    body { margin: 0; }
    main { width: min(960px, calc(100% - 32px)); margin: 0 auto; padding: 32px 0 64px; }
    header, article { background: #181d25; border: 1px solid #2a3340; border-radius: 16px; padding: 22px; margin-bottom: 18px; }
    h1, h2, h3 { margin-top: 0; }
    .muted { color: #aab4c2; }
    .source { line-height: 1.6; white-space: pre-wrap; padding: 14px; background: #11161d; border-radius: 10px; }
    .samples { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
    .sample { padding: 14px; background: #11161d; border-radius: 12px; }
    audio { width: 100%; }
    fieldset { border: 0; padding: 14px 0 0; display: flex; flex-wrap: wrap; gap: 12px 18px; }
    fieldset.dimension { margin-top: 8px; padding: 12px; background: #141a22; border-radius: 10px; }
    fieldset.dimension legend { font-weight: 700; padding: 0 6px; }
    label { cursor: pointer; }
    textarea { box-sizing: border-box; width: 100%; min-height: 72px; margin-top: 12px; padding: 10px; border-radius: 10px; border: 1px solid #354152; background: #0f141a; color: inherit; }
    button { border: 0; border-radius: 10px; padding: 12px 18px; font-weight: 700; cursor: pointer; background: #67e8b2; color: #092217; }
    #status { min-height: 1.5em; color: #aab4c2; }
    @media (max-width: 680px) { .samples { grid-template-columns: 1fr; } }
  </style>
</head>
<body data-session="{{.SessionID}}">
<main>
  <header>
    <p class="muted">Внутренняя субъективная оценка · не официальный MOS</p>
    <h1>Слепое сравнение TTS</h1>
    <p>Названия алгоритмов скрыты. Прослушайте A и B в одинаковых наушниках и громкости. Оценивайте естественность, произношение, интонацию и целостность фразы.</p>
  </header>
  <form id="rating-form">
  {{range $index, $case := .Cases}}
    <article data-case-id="{{$case.CaseID}}">
      <p class="muted">Случай {{add $index 1}} из {{len $.Cases}}</p>
      <h2>{{$case.CaseID}}</h2>
      <p class="source">{{$case.SourceText}}</p>
      <div class="samples">
      {{range $sample := $case.Samples}}
        <section class="sample">
          <h3>Sample {{$sample.BlindLabel}}</h3>
          <audio controls preload="metadata" src="{{$sample.RelativePath}}"></audio>
          <p class="muted">{{$sample.DurationSeconds | seconds}}</p>
        </section>
      {{end}}
      </div>
      <fieldset>
        <legend>Какой вариант лучше?</legend>
        <label><input type="radio" name="{{$case.CaseID}}-verdict" value="prefer_a"> A</label>
        <label><input type="radio" name="{{$case.CaseID}}-verdict" value="prefer_b"> B</label>
        <label><input type="radio" name="{{$case.CaseID}}-verdict" value="tie"> Одинаково</label>
        <label><input type="radio" name="{{$case.CaseID}}-verdict" value="both_bad"> Оба плохие</label>
      </fieldset>
      <fieldset class="dimension" data-dimension="pronunciation">
        <legend>Произношение и ударения</legend>
        <label><input type="radio" name="{{$case.CaseID}}-pronunciation" value="prefer_a"> A</label>
        <label><input type="radio" name="{{$case.CaseID}}-pronunciation" value="prefer_b"> B</label>
        <label><input type="radio" name="{{$case.CaseID}}-pronunciation" value="tie"> Одинаково</label>
        <label><input type="radio" name="{{$case.CaseID}}-pronunciation" value="both_bad"> Оба плохие</label>
      </fieldset>
      <fieldset class="dimension" data-dimension="prosody">
        <legend>Просодия и паузы</legend>
        <label><input type="radio" name="{{$case.CaseID}}-prosody" value="prefer_a"> A</label>
        <label><input type="radio" name="{{$case.CaseID}}-prosody" value="prefer_b"> B</label>
        <label><input type="radio" name="{{$case.CaseID}}-prosody" value="tie"> Одинаково</label>
        <label><input type="radio" name="{{$case.CaseID}}-prosody" value="both_bad"> Оба плохие</label>
      </fieldset>
      <fieldset class="dimension" data-dimension="speaker_stability">
        <legend>Сходство и стабильность голоса</legend>
        <label><input type="radio" name="{{$case.CaseID}}-speaker_stability" value="prefer_a"> A</label>
        <label><input type="radio" name="{{$case.CaseID}}-speaker_stability" value="prefer_b"> B</label>
        <label><input type="radio" name="{{$case.CaseID}}-speaker_stability" value="tie"> Одинаково</label>
        <label><input type="radio" name="{{$case.CaseID}}-speaker_stability" value="both_bad"> Оба плохие</label>
      </fieldset>
      <textarea name="{{$case.CaseID}}-notes" maxlength="1000" placeholder="Что именно отличалось: паузы, инициалы, ритм, интонация…"></textarea>
    </article>
  {{end}}
  </form>
  <button id="download" type="button">Сохранить оценки JSON</button>
  <p id="status" role="status" aria-live="polite"></p>
</main>
<script>
(() => {
  "use strict";
  const form = document.getElementById("rating-form");
  const status = document.getElementById("status");
  const sessionID = document.body.dataset.session;
  const storageKey = "tts-quality-blind:" + sessionID;
  const articles = Array.from(form.querySelectorAll("article[data-case-id]"));

  function snapshot() {
    const state = {};
    for (const article of articles) {
      const caseID = article.dataset.caseId;
      const selected = article.querySelector("input[type=radio]:checked");
      const dimensions = {};
      for (const fieldset of article.querySelectorAll("fieldset[data-dimension]")) {
        const dimensionSelected = fieldset.querySelector("input[type=radio]:checked");
        dimensions[fieldset.dataset.dimension] = dimensionSelected ? dimensionSelected.value : "";
      }
      state[caseID] = {
        verdict: selected ? selected.value : "",
        dimensions,
        notes: article.querySelector("textarea").value,
      };
    }
    return state;
  }

  function persist() {
    localStorage.setItem(storageKey, JSON.stringify(snapshot()));
    status.textContent = "Черновик сохранён в браузере.";
  }

  function restore() {
    let state;
    try { state = JSON.parse(localStorage.getItem(storageKey) || "{}"); } catch (_) { state = {}; }
    for (const article of articles) {
      const saved = state[article.dataset.caseId];
      if (!saved) continue;
      const radio = Array.from(article.querySelectorAll("input[type=radio]"))
        .find(input => input.value === saved.verdict);
      if (radio) radio.checked = true;
      for (const fieldset of article.querySelectorAll("fieldset[data-dimension]")) {
        const value = saved.dimensions && saved.dimensions[fieldset.dataset.dimension];
        const dimensionRadio = Array.from(fieldset.querySelectorAll("input[type=radio]"))
          .find(input => input.value === value);
        if (dimensionRadio) dimensionRadio.checked = true;
      }
      article.querySelector("textarea").value = saved.notes || "";
    }
  }

  form.addEventListener("input", persist);
  restore();
  document.getElementById("download").addEventListener("click", () => {
    const state = snapshot();
    const missing = articles.filter(article => {
      const saved = state[article.dataset.caseId];
      return !saved.verdict || Object.values(saved.dimensions).some(value => !value);
    });
    if (missing.length) {
      status.textContent = "Оцените все случаи. Осталось: " + missing.length + ".";
      missing[0].scrollIntoView({behavior: "smooth", block: "center"});
      return;
    }
    const cases = articles.map(article => {
      const caseID = article.dataset.caseId;
      const saved = state[caseID];
      const preferred = saved.verdict === "prefer_a" ? "A" : saved.verdict === "prefer_b" ? "B" : null;
      const dimensionRatings = {};
      for (const [dimension, verdict] of Object.entries(saved.dimensions)) {
        dimensionRatings[dimension] = {
          preferred_label: verdict === "prefer_a" ? "A" : verdict === "prefer_b" ? "B" : null,
          verdict: verdict === "prefer_a" || verdict === "prefer_b" ? "preference" : verdict,
        };
      }
      return {
        case_id: caseID,
        preferred_label: preferred,
        verdict: preferred ? "preference" : saved.verdict,
        dimension_ratings: dimensionRatings,
        notes: saved.notes,
      };
    });
    const result = {
      schema_version: "tts-quality-blind-session-v1",
      session_id: sessionID,
      rated_at: new Date().toISOString(),
      method: "single-listener blind A/B preference; not MOS",
      reveal_mapping_path: "reveal.json",
      cases,
    };
    const blob = new Blob([JSON.stringify(result, null, 2) + "\n"], {type: "application/json"});
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = "ratings-" + sessionID + ".json";
    link.click();
    URL.revokeObjectURL(url);
    status.textContent = "Оценки сохранены. Reveal mapping пока не открывайте.";
  });
})();
</script>
</body>
</html>`))
