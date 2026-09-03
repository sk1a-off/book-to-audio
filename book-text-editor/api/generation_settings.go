package api

import (
	"fmt"
	"math"
	"net/http"

	"book-text-editor/internal/russiantext"
)

const (
	defaultAutomaticWarningRetries = 1
	defaultWhisperBeamSize         = 5
	defaultWhisperPatience         = 1.0
	defaultWhisperTemperature      = 0.0
	defaultWhisperVADFilter        = false
	defaultWhisperWordTimestamps   = true

	defaultTTSDenoise             = true
	defaultTTSTShift              = 0.1
	defaultTTSLayerPenaltyFactor  = 5.0
	defaultTTSPositionTemperature = 5.0
	defaultTTSClassTemperature    = 0.0
	defaultTTSPreprocessPrompt    = true
	defaultTTSPostprocessOutput   = true
	defaultTTSAudioChunkDuration  = 15.0
	defaultTTSAudioChunkThreshold = 30.0
	defaultTTSPadDuration         = 0.1
	defaultTTSFadeDuration        = 0.1
)

// PronunciationGenerationSettings contains sparse, job-scoped pronunciation
// hints. Rules use one pair per line: "source => source with U+0301 accents".
// The canonical fragment text is never replaced by this snapshot.
type PronunciationGenerationSettings struct {
	Enabled bool   `json:"enabled"`
	Rules   string `json:"rules"`
}

// RussianTextGenerationSettings controls deterministic preparation of the
// OmniVoice-only text representation. Version is server-owned and persisted
// with the job so a resume cannot silently switch rulesets.
type RussianTextGenerationSettings struct {
	Version             string `json:"version"`
	SelectiveStress     bool   `json:"selective_stress"`
	NormalizeMorphology bool   `json:"normalize_morphology"`
}

// OmniVoiceGenerationSettings is the resolved, immutable worker configuration
// stored with a generation job.
type OmniVoiceGenerationSettings struct {
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

// WhisperGenerationSettings contains request-scoped inference settings. Model,
// device and process settings deliberately stay outside of job snapshots.
type WhisperGenerationSettings struct {
	BeamSize       int     `json:"beam_size"`
	Patience       float64 `json:"patience"`
	Temperature    float64 `json:"temperature"`
	VADFilter      bool    `json:"vad_filter"`
	WordTimestamps bool    `json:"word_timestamps"`
}

// GenerationSettings is the resolved generation policy snapshot stored with a
// job and returned by job APIs.
type GenerationSettings struct {
	OmniVoice               OmniVoiceGenerationSettings     `json:"omnivoice"`
	Whisper                 WhisperGenerationSettings       `json:"whisper"`
	Pronunciation           PronunciationGenerationSettings `json:"pronunciation"`
	RussianText             RussianTextGenerationSettings   `json:"russian_text"`
	AutomaticWarningRetries int                             `json:"automatic_warning_retries"`
}

type generationSettingsInput struct {
	OmniVoice               *omniVoiceGenerationSettingsInput     `json:"omnivoice,omitempty"`
	Whisper                 *whisperGenerationSettingsInput       `json:"whisper,omitempty"`
	Pronunciation           *pronunciationGenerationSettingsInput `json:"pronunciation,omitempty"`
	RussianText             *russianTextGenerationSettingsInput   `json:"russian_text,omitempty"`
	AutomaticWarningRetries *int                                  `json:"automatic_warning_retries,omitempty"`
}

type pronunciationGenerationSettingsInput struct {
	Enabled *bool   `json:"enabled,omitempty"`
	Rules   *string `json:"rules,omitempty"`
}

type russianTextGenerationSettingsInput struct {
	SelectiveStress     *bool `json:"selective_stress,omitempty"`
	NormalizeMorphology *bool `json:"normalize_morphology,omitempty"`
}

type omniVoiceGenerationSettingsInput struct {
	NumSteps            *int     `json:"num_steps,omitempty"`
	GuidanceScale       *float64 `json:"guidance_scale,omitempty"`
	Speed               *float64 `json:"speed,omitempty"`
	NormalizeText       *bool    `json:"normalize_text,omitempty"`
	Denoise             *bool    `json:"denoise,omitempty"`
	TShift              *float64 `json:"t_shift,omitempty"`
	LayerPenaltyFactor  *float64 `json:"layer_penalty_factor,omitempty"`
	PositionTemperature *float64 `json:"position_temperature,omitempty"`
	ClassTemperature    *float64 `json:"class_temperature,omitempty"`
	PreprocessPrompt    *bool    `json:"preprocess_prompt,omitempty"`
	PostprocessOutput   *bool    `json:"postprocess_output,omitempty"`
	AudioChunkDuration  *float64 `json:"audio_chunk_duration,omitempty"`
	AudioChunkThreshold *float64 `json:"audio_chunk_threshold,omitempty"`
	PadDuration         *float64 `json:"pad_duration,omitempty"`
	FadeDuration        *float64 `json:"fade_duration,omitempty"`
}

type whisperGenerationSettingsInput struct {
	BeamSize       *int     `json:"beam_size,omitempty"`
	Patience       *float64 `json:"patience,omitempty"`
	Temperature    *float64 `json:"temperature,omitempty"`
	VADFilter      *bool    `json:"vad_filter,omitempty"`
	WordTimestamps *bool    `json:"word_timestamps,omitempty"`
}

func defaultGenerationSettings() GenerationSettings {
	return GenerationSettings{
		OmniVoice: OmniVoiceGenerationSettings{
			NumSteps:            defaultTTSNumSteps,
			GuidanceScale:       defaultTTSGuidanceScale,
			Speed:               defaultTTSSpeed,
			NormalizeText:       true,
			Denoise:             defaultTTSDenoise,
			TShift:              defaultTTSTShift,
			LayerPenaltyFactor:  defaultTTSLayerPenaltyFactor,
			PositionTemperature: defaultTTSPositionTemperature,
			ClassTemperature:    defaultTTSClassTemperature,
			PreprocessPrompt:    defaultTTSPreprocessPrompt,
			PostprocessOutput:   defaultTTSPostprocessOutput,
			AudioChunkDuration:  defaultTTSAudioChunkDuration,
			AudioChunkThreshold: defaultTTSAudioChunkThreshold,
			PadDuration:         defaultTTSPadDuration,
			FadeDuration:        defaultTTSFadeDuration,
		},
		Whisper: WhisperGenerationSettings{
			BeamSize:       defaultWhisperBeamSize,
			Patience:       defaultWhisperPatience,
			Temperature:    defaultWhisperTemperature,
			VADFilter:      defaultWhisperVADFilter,
			WordTimestamps: defaultWhisperWordTimestamps,
		},
		RussianText: RussianTextGenerationSettings{
			Version:             russiantext.Version,
			SelectiveStress:     true,
			NormalizeMorphology: true,
		},
		AutomaticWarningRetries: defaultAutomaticWarningRetries,
	}
}

func decodeGenerationSettings(
	writer http.ResponseWriter,
	request *http.Request,
) (GenerationSettings, bool) {
	var input generationSettingsInput
	if !decodeJSON(writer, request, &input, true) {
		return GenerationSettings{}, false
	}

	settings, err := resolveGenerationSettings(input)
	if err != nil {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_GENERATION_SETTINGS",
			err.Error(),
		)
		return GenerationSettings{}, false
	}
	return settings, true
}

func resolveGenerationSettings(
	input generationSettingsInput,
) (GenerationSettings, error) {
	settings := defaultGenerationSettings()
	if input.OmniVoice != nil {
		if input.OmniVoice.NumSteps != nil {
			settings.OmniVoice.NumSteps = *input.OmniVoice.NumSteps
		}
		if input.OmniVoice.GuidanceScale != nil {
			settings.OmniVoice.GuidanceScale =
				*input.OmniVoice.GuidanceScale
		}
		if input.OmniVoice.Speed != nil {
			settings.OmniVoice.Speed = *input.OmniVoice.Speed
		}
		if input.OmniVoice.NormalizeText != nil {
			settings.OmniVoice.NormalizeText =
				*input.OmniVoice.NormalizeText
		}
		if input.OmniVoice.Denoise != nil {
			settings.OmniVoice.Denoise = *input.OmniVoice.Denoise
		}
		if input.OmniVoice.TShift != nil {
			settings.OmniVoice.TShift = *input.OmniVoice.TShift
		}
		if input.OmniVoice.LayerPenaltyFactor != nil {
			settings.OmniVoice.LayerPenaltyFactor =
				*input.OmniVoice.LayerPenaltyFactor
		}
		if input.OmniVoice.PositionTemperature != nil {
			settings.OmniVoice.PositionTemperature =
				*input.OmniVoice.PositionTemperature
		}
		if input.OmniVoice.ClassTemperature != nil {
			settings.OmniVoice.ClassTemperature =
				*input.OmniVoice.ClassTemperature
		}
		if input.OmniVoice.PreprocessPrompt != nil {
			settings.OmniVoice.PreprocessPrompt =
				*input.OmniVoice.PreprocessPrompt
		}
		if input.OmniVoice.PostprocessOutput != nil {
			settings.OmniVoice.PostprocessOutput =
				*input.OmniVoice.PostprocessOutput
		}
		if input.OmniVoice.AudioChunkDuration != nil {
			settings.OmniVoice.AudioChunkDuration =
				*input.OmniVoice.AudioChunkDuration
		}
		if input.OmniVoice.AudioChunkThreshold != nil {
			settings.OmniVoice.AudioChunkThreshold =
				*input.OmniVoice.AudioChunkThreshold
		}
		if input.OmniVoice.PadDuration != nil {
			settings.OmniVoice.PadDuration =
				*input.OmniVoice.PadDuration
		}
		if input.OmniVoice.FadeDuration != nil {
			settings.OmniVoice.FadeDuration =
				*input.OmniVoice.FadeDuration
		}
	}
	if input.Whisper != nil {
		if input.Whisper.BeamSize != nil {
			settings.Whisper.BeamSize = *input.Whisper.BeamSize
		}
		if input.Whisper.Patience != nil {
			settings.Whisper.Patience = *input.Whisper.Patience
		}
		if input.Whisper.Temperature != nil {
			settings.Whisper.Temperature =
				*input.Whisper.Temperature
		}
		if input.Whisper.VADFilter != nil {
			settings.Whisper.VADFilter = *input.Whisper.VADFilter
		}
		if input.Whisper.WordTimestamps != nil {
			settings.Whisper.WordTimestamps =
				*input.Whisper.WordTimestamps
		}
	}
	if input.Pronunciation != nil {
		if input.Pronunciation.Enabled != nil {
			settings.Pronunciation.Enabled = *input.Pronunciation.Enabled
		}
		if input.Pronunciation.Rules != nil {
			settings.Pronunciation.Rules = *input.Pronunciation.Rules
		}
	}
	if input.RussianText != nil {
		if input.RussianText.SelectiveStress != nil {
			settings.RussianText.SelectiveStress =
				*input.RussianText.SelectiveStress
		}
		if input.RussianText.NormalizeMorphology != nil {
			settings.RussianText.NormalizeMorphology =
				*input.RussianText.NormalizeMorphology
		}
	}
	if input.AutomaticWarningRetries != nil {
		settings.AutomaticWarningRetries = *input.AutomaticWarningRetries
	}
	if err := validateGenerationSettings(settings); err != nil {
		return GenerationSettings{}, err
	}
	return settings, nil
}

func validateGenerationSettings(settings GenerationSettings) error {
	if settings.OmniVoice.NumSteps < 1 ||
		settings.OmniVoice.NumSteps > 100 {
		return fmt.Errorf("omnivoice.num_steps must be between 1 and 100")
	}
	if !finiteGenerationSetting(settings.OmniVoice.GuidanceScale) ||
		settings.OmniVoice.GuidanceScale < 0 ||
		settings.OmniVoice.GuidanceScale > 10 {
		return fmt.Errorf(
			"omnivoice.guidance_scale must be between 0 and 10",
		)
	}
	if !finiteGenerationSetting(settings.OmniVoice.Speed) ||
		settings.OmniVoice.Speed < 0.5 ||
		settings.OmniVoice.Speed > 2 {
		return fmt.Errorf("omnivoice.speed must be between 0.5 and 2")
	}
	if !finiteGenerationSetting(settings.OmniVoice.TShift) ||
		settings.OmniVoice.TShift < 0.001 ||
		settings.OmniVoice.TShift > 10 {
		return fmt.Errorf("omnivoice.t_shift must be between 0.001 and 10")
	}
	if !finiteGenerationSetting(settings.OmniVoice.LayerPenaltyFactor) ||
		settings.OmniVoice.LayerPenaltyFactor < 0 ||
		settings.OmniVoice.LayerPenaltyFactor > 20 {
		return fmt.Errorf(
			"omnivoice.layer_penalty_factor must be between 0 and 20",
		)
	}
	if !finiteGenerationSetting(settings.OmniVoice.PositionTemperature) ||
		settings.OmniVoice.PositionTemperature < 0 ||
		settings.OmniVoice.PositionTemperature > 20 {
		return fmt.Errorf(
			"omnivoice.position_temperature must be between 0 and 20",
		)
	}
	if !finiteGenerationSetting(settings.OmniVoice.ClassTemperature) ||
		settings.OmniVoice.ClassTemperature < 0 ||
		settings.OmniVoice.ClassTemperature > 10 {
		return fmt.Errorf(
			"omnivoice.class_temperature must be between 0 and 10",
		)
	}
	if !finiteGenerationSetting(settings.OmniVoice.AudioChunkDuration) ||
		settings.OmniVoice.AudioChunkDuration < 1 ||
		settings.OmniVoice.AudioChunkDuration > 120 {
		return fmt.Errorf(
			"omnivoice.audio_chunk_duration must be between 1 and 120",
		)
	}
	if !finiteGenerationSetting(settings.OmniVoice.AudioChunkThreshold) ||
		settings.OmniVoice.AudioChunkThreshold < 1 ||
		settings.OmniVoice.AudioChunkThreshold > 600 {
		return fmt.Errorf(
			"omnivoice.audio_chunk_threshold must be between 1 and 600",
		)
	}
	if !finiteGenerationSetting(settings.OmniVoice.PadDuration) ||
		settings.OmniVoice.PadDuration < 0 ||
		settings.OmniVoice.PadDuration > 5 {
		return fmt.Errorf(
			"omnivoice.pad_duration must be between 0 and 5",
		)
	}
	if !finiteGenerationSetting(settings.OmniVoice.FadeDuration) ||
		settings.OmniVoice.FadeDuration < 0 ||
		settings.OmniVoice.FadeDuration > 5 {
		return fmt.Errorf(
			"omnivoice.fade_duration must be between 0 and 5",
		)
	}
	if settings.Whisper.BeamSize < 1 ||
		settings.Whisper.BeamSize > 10 {
		return fmt.Errorf("whisper.beam_size must be between 1 and 10")
	}
	if !finiteGenerationSetting(settings.Whisper.Patience) ||
		settings.Whisper.Patience < 0.1 ||
		settings.Whisper.Patience > 2 {
		return fmt.Errorf("whisper.patience must be between 0.1 and 2")
	}
	if !finiteGenerationSetting(settings.Whisper.Temperature) ||
		settings.Whisper.Temperature < 0 ||
		settings.Whisper.Temperature > 1 {
		return fmt.Errorf("whisper.temperature must be between 0 and 1")
	}
	if settings.AutomaticWarningRetries < 0 ||
		settings.AutomaticWarningRetries > 10 {
		return fmt.Errorf(
			"automatic_warning_retries must be between 0 and 10",
		)
	}
	if _, err := parsePronunciationRules(settings.Pronunciation.Rules); err != nil {
		return fmt.Errorf("pronunciation.rules: %w", err)
	}
	if settings.RussianText.Version != russiantext.Version {
		return fmt.Errorf(
			"russian_text.version %q is not supported by this build",
			settings.RussianText.Version,
		)
	}
	return nil
}

func finiteGenerationSetting(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
