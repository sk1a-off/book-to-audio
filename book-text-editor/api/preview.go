package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	defaultPreviewSeed = uint32(42)
	minPreviewRunes    = 20
	maxPreviewRunes    = 2_000
)

type previewInput struct {
	Text               string                  `json:"text"`
	Seed               *uint32                 `json:"seed,omitempty"`
	GenerationSettings generationSettingsInput `json:"generation_settings"`
}

func (s *Server) generatePreview(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var input previewInput
	if !decodeJSON(writer, request, &input, false) {
		return
	}
	input.Text = strings.TrimSpace(input.Text)
	textRunes := utf8.RuneCountInString(input.Text)
	if textRunes < minPreviewRunes || textRunes > maxPreviewRunes {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_PREVIEW_TEXT",
			fmt.Sprintf(
				"preview text must contain between %d and %d characters",
				minPreviewRunes,
				maxPreviewRunes,
			),
		)
		return
	}
	settings, err := resolveGenerationSettings(input.GenerationSettings)
	if err != nil {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_GENERATION_SETTINGS",
			err.Error(),
		)
		return
	}
	if s.previewSlot != nil {
		select {
		case s.previewSlot <- struct{}{}:
			defer func() { <-s.previewSlot }()
		default:
			writer.Header().Set("Retry-After", "5")
			writeProblem(
				writer,
				request,
				http.StatusTooManyRequests,
				"PREVIEW_BUSY",
				"another quality preview is already waiting or running",
			)
			return
		}
	}
	voice, ok, err := s.store.voicePayload(
		request.Context(),
		request.PathValue("voiceID"),
	)
	if err != nil {
		s.internalStoreError(writer, request, "get preview voice", err)
		return
	}
	if !ok {
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"VOICE_NOT_FOUND",
			"voice not found",
		)
		return
	}
	ttsText, preparation, err := prepareRussianTTSText(
		input.Text,
		settings,
	)
	if err != nil {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_GENERATION_SETTINGS",
			err.Error(),
		)
		return
	}
	seed := defaultPreviewSeed
	if input.Seed != nil {
		seed = *input.Seed
	}

	workerContext, cancel := s.workerContext(request.Context())
	defer cancel()
	result, err := s.generateTTS(workerContext, TTSRequest{
		RequestID:            s.newID(),
		JobID:                "preview",
		FragmentID:           s.newID(),
		Text:                 ttsText,
		ReferenceAudio:       voice.Audio,
		ReferenceContentType: voice.Resource.ContentType,
		ReferenceText:        voice.ReferenceText,
		NumSteps:             settings.OmniVoice.NumSteps,
		GuidanceScale:        settings.OmniVoice.GuidanceScale,
		Speed:                settings.OmniVoice.Speed,
		NormalizeText:        settings.OmniVoice.NormalizeText,
		Denoise:              settings.OmniVoice.Denoise,
		TShift:               settings.OmniVoice.TShift,
		LayerPenaltyFactor:   settings.OmniVoice.LayerPenaltyFactor,
		PositionTemperature:  settings.OmniVoice.PositionTemperature,
		ClassTemperature:     settings.OmniVoice.ClassTemperature,
		PreprocessPrompt:     settings.OmniVoice.PreprocessPrompt,
		PostprocessOutput:    settings.OmniVoice.PostprocessOutput,
		AudioChunkDuration:   settings.OmniVoice.AudioChunkDuration,
		AudioChunkThreshold:  settings.OmniVoice.AudioChunkThreshold,
		PadDuration:          settings.OmniVoice.PadDuration,
		FadeDuration:         settings.OmniVoice.FadeDuration,
		Seed:                 &seed,
		SettingsResolved:     true,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || request.Context().Err() != nil {
			return
		}
		s.logger.Error(
			"generate quality preview",
			"request_id", requestID(request.Context()),
			"voice_id", voice.Resource.ID,
			"error", err,
		)
		writeProblem(
			writer,
			request,
			http.StatusBadGateway,
			"PREVIEW_GENERATION_FAILED",
			"OmniVoice preview generation failed",
		)
		return
	}
	wav, err := encodePCMAsWAV(
		result.AudioPCM,
		result.SampleRate,
		result.Channels,
		result.SampleWidth,
	)
	if err != nil {
		s.logger.Error(
			"encode quality preview",
			"request_id", requestID(request.Context()),
			"error", err,
		)
		writeProblem(
			writer,
			request,
			http.StatusBadGateway,
			"INVALID_PREVIEW_AUDIO",
			"OmniVoice returned invalid preview audio",
		)
		return
	}

	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "audio/wav")
	writer.Header().Set("Content-Disposition", `inline; filename="quality-preview.wav"`)
	writer.Header().Set("Content-Length", strconv.Itoa(len(wav)))
	writer.Header().Set("X-TTS-Seed", strconv.FormatUint(uint64(seed), 10))
	writer.Header().Set("X-Audio-Duration-Ms", strconv.Itoa(result.DurationMS))
	writer.Header().Set(
		"X-Pronunciation-Rules-Applied",
		strconv.Itoa(preparation.ManualPronunciationRules),
	)
	writer.Header().Set(
		"X-Selective-Stress-Rules-Applied",
		strconv.Itoa(preparation.SelectiveStressRules),
	)
	writer.Header().Set(
		"X-Morphology-Replacements",
		strconv.Itoa(preparation.MorphologyReplacements),
	)
	writer.Header().Set("X-Russian-Text-Version", preparation.Version)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(wav)
}

func (s *Server) generateTTS(
	ctx context.Context,
	request TTSRequest,
) (TTSResult, error) {
	if s.ttsSlot == nil {
		return s.tts.Generate(ctx, request)
	}
	select {
	case s.ttsSlot <- struct{}{}:
		defer func() { <-s.ttsSlot }()
	case <-ctx.Done():
		return TTSResult{}, ctx.Err()
	}
	return s.tts.Generate(ctx, request)
}
