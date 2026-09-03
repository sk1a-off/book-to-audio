package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"book-text-editor/internal/book"
)

func TestDecodeGenerationSettings(t *testing.T) {
	t.Parallel()

	t.Run("empty body uses defaults", func(t *testing.T) {
		t.Parallel()

		request := httptest.NewRequest(http.MethodPost, "/generate", nil)
		response := httptest.NewRecorder()
		got, ok := decodeGenerationSettings(response, request)

		if !ok || response.Code != http.StatusOK {
			t.Fatalf(
				"decodeGenerationSettings() = (%+v, %v), status %d",
				got,
				ok,
				response.Code,
			)
		}
		if want := defaultGenerationSettings(); got != want {
			t.Fatalf("settings = %+v, want %+v", got, want)
		}
		if !got.RussianText.SelectiveStress ||
			!got.RussianText.NormalizeMorphology {
			t.Fatalf(
				"russian text defaults = %+v, want validated v3 features enabled",
				got.RussianText,
			)
		}
	})

	t.Run("custom values preserve explicit zero and false", func(t *testing.T) {
		t.Parallel()

		body := `{
			"omnivoice":{
				"num_steps":8,
				"guidance_scale":0,
				"speed":1.25,
				"normalize_text":false,
				"denoise":false,
				"t_shift":0.2,
				"layer_penalty_factor":4.5,
				"position_temperature":3.5,
				"class_temperature":0.25,
				"preprocess_prompt":false,
				"postprocess_output":false,
				"audio_chunk_duration":12,
				"audio_chunk_threshold":25,
				"pad_duration":0,
				"fade_duration":0.05
			},
			"whisper":{
				"beam_size":7,
				"patience":1.5,
				"temperature":0,
				"vad_filter":true,
				"word_timestamps":false
			},
			"russian_text":{
				"selective_stress":true,
				"normalize_morphology":true
			},
			"automatic_warning_retries":0
		}`
		request := httptest.NewRequest(
			http.MethodPost,
			"/generate",
			strings.NewReader(body),
		)
		response := httptest.NewRecorder()
		got, ok := decodeGenerationSettings(response, request)

		want := GenerationSettings{
			OmniVoice: OmniVoiceGenerationSettings{
				NumSteps:            8,
				GuidanceScale:       0,
				Speed:               1.25,
				NormalizeText:       false,
				Denoise:             false,
				TShift:              0.2,
				LayerPenaltyFactor:  4.5,
				PositionTemperature: 3.5,
				ClassTemperature:    0.25,
				PreprocessPrompt:    false,
				PostprocessOutput:   false,
				AudioChunkDuration:  12,
				AudioChunkThreshold: 25,
				PadDuration:         0,
				FadeDuration:        0.05,
			},
			Whisper: WhisperGenerationSettings{
				BeamSize:       7,
				Patience:       1.5,
				Temperature:    0,
				VADFilter:      true,
				WordTimestamps: false,
			},
			RussianText: RussianTextGenerationSettings{
				Version:             "ru-selective-morph-v3",
				SelectiveStress:     true,
				NormalizeMorphology: true,
			},
			AutomaticWarningRetries: 0,
		}
		if !ok || response.Code != http.StatusOK || got != want {
			t.Fatalf(
				"decodeGenerationSettings() = (%+v, %v), status %d; want %+v",
				got,
				ok,
				response.Code,
				want,
			)
		}
	})

	t.Run("unknown field is rejected", func(t *testing.T) {
		t.Parallel()

		request := httptest.NewRequest(
			http.MethodPost,
			"/generate",
			strings.NewReader(`{"unknown":true}`),
		)
		response := httptest.NewRecorder()
		_, ok := decodeGenerationSettings(response, request)

		if ok || response.Code != http.StatusBadRequest {
			t.Fatalf("ok = %v, status = %d, want false/400", ok, response.Code)
		}
		var problem ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
		if problem.Code != "INVALID_JSON" {
			t.Fatalf("problem code = %q, want INVALID_JSON", problem.Code)
		}
	})

	invalidBodies := []string{
		`{"omnivoice":{"num_steps":0}}`,
		`{"omnivoice":{"num_steps":101}}`,
		`{"omnivoice":{"guidance_scale":-0.1}}`,
		`{"omnivoice":{"guidance_scale":10.1}}`,
		`{"omnivoice":{"speed":0.49}}`,
		`{"omnivoice":{"speed":2.01}}`,
		`{"omnivoice":{"t_shift":0}}`,
		`{"omnivoice":{"t_shift":10.1}}`,
		`{"omnivoice":{"layer_penalty_factor":-0.1}}`,
		`{"omnivoice":{"layer_penalty_factor":20.1}}`,
		`{"omnivoice":{"position_temperature":-0.1}}`,
		`{"omnivoice":{"position_temperature":20.1}}`,
		`{"omnivoice":{"class_temperature":-0.1}}`,
		`{"omnivoice":{"class_temperature":10.1}}`,
		`{"omnivoice":{"audio_chunk_duration":0.9}}`,
		`{"omnivoice":{"audio_chunk_duration":120.1}}`,
		`{"omnivoice":{"audio_chunk_threshold":0.9}}`,
		`{"omnivoice":{"audio_chunk_threshold":600.1}}`,
		`{"omnivoice":{"pad_duration":-0.1}}`,
		`{"omnivoice":{"pad_duration":5.1}}`,
		`{"omnivoice":{"fade_duration":-0.1}}`,
		`{"omnivoice":{"fade_duration":5.1}}`,
		`{"whisper":{"beam_size":0}}`,
		`{"whisper":{"beam_size":11}}`,
		`{"whisper":{"patience":0.09}}`,
		`{"whisper":{"patience":2.01}}`,
		`{"whisper":{"temperature":-0.01}}`,
		`{"whisper":{"temperature":1.01}}`,
		`{"automatic_warning_retries":-1}`,
		`{"automatic_warning_retries":11}`,
	}
	for index, body := range invalidBodies {
		body := body
		t.Run(fmt.Sprintf("invalid range %d", index), func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequest(
				http.MethodPost,
				"/generate",
				strings.NewReader(body),
			)
			response := httptest.NewRecorder()
			_, ok := decodeGenerationSettings(response, request)

			if ok || response.Code != http.StatusUnprocessableEntity {
				t.Fatalf(
					"ok = %v, status = %d, want false/422; body=%s",
					ok,
					response.Code,
					body,
				)
			}
		})
	}
}

type retryTestTTS struct {
	requests       []TTSRequest
	warningsBefore int
	err            error
}

func (f *retryTestTTS) Generate(
	ctx context.Context,
	request TTSRequest,
) (TTSResult, error) {
	if err := ctx.Err(); err != nil {
		return TTSResult{}, err
	}
	request.ReferenceAudio = slices.Clone(request.ReferenceAudio)
	if request.Seed != nil {
		seed := *request.Seed
		request.Seed = &seed
	}
	f.requests = append(f.requests, request)
	if f.err != nil {
		return TTSResult{}, f.err
	}

	var warnings []string
	if len(f.requests) <= f.warningsBefore {
		warnings = []string{"clipping"}
	}
	return TTSResult{
		RequestID:   request.RequestID,
		AudioPCM:    []byte{0, 0, 1, 0},
		SampleRate:  24_000,
		Channels:    1,
		SampleWidth: 2,
		DurationMS:  1,
		Warnings:    warnings,
	}, nil
}

type retryTestSTT struct {
	requests         []STTRequest
	mismatchesBefore int
	err              error
}

func (f *retryTestSTT) Transcribe(
	ctx context.Context,
	request STTRequest,
) (STTResult, error) {
	if err := ctx.Err(); err != nil {
		return STTResult{}, err
	}
	request.AudioPCM = slices.Clone(request.AudioPCM)
	f.requests = append(f.requests, request)
	if f.err != nil {
		return STTResult{}, f.err
	}

	transcript := request.ExpectedText
	if len(f.requests) <= f.mismatchesBefore {
		transcript = "распознано неверно"
	}
	return STTResult{
		RequestID: request.RequestID,
		Text:      transcript,
		Language:  "ru",
	}, nil
}

type retryTestResult struct {
	store    *memoryStore
	tts      *retryTestTTS
	stt      *retryTestSTT
	job      JobResource
	fragment FragmentResource
	history  []fragmentAttempt
}

func runRetryTest(
	t *testing.T,
	settings GenerationSettings,
	tts *retryTestTTS,
	stt *retryTestSTT,
) retryTestResult {
	t.Helper()

	ctx := context.Background()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	_, err := store.createBook(ctx, "book-1", book.Book{
		Title: "Книга",
		Chapters: []book.Chapter{{
			Number:   1,
			Title:    "Глава",
			Segments: []string{"Тестовый фрагмент."},
		}},
	}, now)
	if err != nil {
		t.Fatalf("createBook() error = %v", err)
	}
	_, err = store.createVoice(
		ctx,
		"voice-1",
		"Голос",
		"wav",
		"audio/wav",
		"Эталон.",
		[]byte("reference"),
		now,
	)
	if err != nil {
		t.Fatalf("createVoice() error = %v", err)
	}
	_, task, err := store.createJob(
		ctx,
		"job-1",
		"book-1",
		"voice-1",
		[]string{"fragment-1"},
		[]string{"revision-1"},
		settings,
		now,
	)
	if err != nil {
		t.Fatalf("createJob() error = %v", err)
	}

	nextID := 0
	server := &Server{
		store:         store,
		tts:           tts,
		stt:           stt,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		newID:         func() string { nextID++; return fmt.Sprintf("request-%d", nextID) },
		now:           func() time.Time { return now },
		workerTimeout: time.Second,
	}
	runner := &jobRunner{server: server, ctx: ctx}
	server.runner = runner
	runner.process(task)

	jobResource, ok, err := store.job(ctx, "job-1")
	if err != nil || !ok {
		t.Fatalf("job() = (%+v, %v, %v)", jobResource, ok, err)
	}
	store.mu.RLock()
	fragment := store.fragments["fragment-1"].Resource
	history := slices.Clone(store.fragments["fragment-1"].History)
	store.mu.RUnlock()

	return retryTestResult{
		store:    store,
		tts:      tts,
		stt:      stt,
		job:      jobResource,
		fragment: fragment,
		history:  history,
	}
}

func TestAutomaticWarningRetriesExhaustConfiguredBudget(t *testing.T) {
	t.Parallel()

	settings := defaultGenerationSettings()
	result := runRetryTest(
		t,
		settings,
		&retryTestTTS{},
		&retryTestSTT{mismatchesBefore: 100},
	)

	if len(result.tts.requests) != 2 ||
		len(result.stt.requests) != 2 ||
		len(result.history) != 2 {
		t.Fatalf(
			"attempts = TTS:%d STT:%d history:%d, want 2 each",
			len(result.tts.requests),
			len(result.stt.requests),
			len(result.history),
		)
	}
	if result.job.Status != JobStatusCompletedWithWarnings ||
		result.job.GenerationSettings != settings ||
		result.fragment.Status != FragmentStatusWarning ||
		result.fragment.WarningCode != "transcript_mismatch" ||
		result.fragment.Attempt != 2 {
		t.Fatalf("final state = job:%+v fragment:%+v", result.job, result.fragment)
	}

	requestIDs := make(map[string]struct{}, 4)
	seeds := make(map[uint32]struct{}, 2)
	for index, request := range result.tts.requests {
		if request.Seed == nil {
			t.Fatalf("TTS request %d has no seed", index)
		}
		if _, duplicate := seeds[*request.Seed]; duplicate {
			t.Fatalf("TTS seed %d is duplicated", *request.Seed)
		}
		seeds[*request.Seed] = struct{}{}
		requestIDs[request.RequestID] = struct{}{}
		assertRetryTestTTSSettings(t, request, settings)
	}
	for _, request := range result.stt.requests {
		requestIDs[request.RequestID] = struct{}{}
		assertRetryTestSTTSettings(t, request, settings)
	}
	if len(requestIDs) != 4 {
		t.Fatalf("unique worker request IDs = %d, want 4", len(requestIDs))
	}
}

func TestAutomaticWarningRetriesStopAfterEarlySuccess(t *testing.T) {
	t.Parallel()

	result := runRetryTest(
		t,
		defaultGenerationSettings(),
		&retryTestTTS{},
		&retryTestSTT{mismatchesBefore: 1},
	)

	if len(result.tts.requests) != 2 ||
		len(result.stt.requests) != 2 ||
		len(result.history) != 2 {
		t.Fatalf(
			"attempts = TTS:%d STT:%d history:%d, want 2 each",
			len(result.tts.requests),
			len(result.stt.requests),
			len(result.history),
		)
	}
	if result.job.Status != JobStatusCompleted ||
		result.fragment.Status != FragmentStatusReady ||
		result.fragment.WarningCode != "" ||
		result.fragment.Attempt != 2 {
		t.Fatalf("final state = job:%+v fragment:%+v", result.job, result.fragment)
	}
}

func TestAutomaticWarningRetriesCanBeDisabled(t *testing.T) {
	t.Parallel()

	settings := defaultGenerationSettings()
	settings.OmniVoice.NumSteps = 8
	settings.OmniVoice.GuidanceScale = 0
	settings.OmniVoice.Speed = 1.25
	settings.OmniVoice.NormalizeText = false
	settings.Whisper.BeamSize = 7
	settings.Whisper.Patience = 1.5
	settings.Whisper.Temperature = 0.25
	settings.Whisper.VADFilter = true
	settings.Whisper.WordTimestamps = false
	settings.AutomaticWarningRetries = 0
	result := runRetryTest(
		t,
		settings,
		&retryTestTTS{},
		&retryTestSTT{mismatchesBefore: 100},
	)

	if len(result.tts.requests) != 1 ||
		len(result.stt.requests) != 1 ||
		len(result.history) != 1 {
		t.Fatalf(
			"attempts = TTS:%d STT:%d history:%d, want 1 each",
			len(result.tts.requests),
			len(result.stt.requests),
			len(result.history),
		)
	}
	assertRetryTestTTSSettings(t, result.tts.requests[0], settings)
	assertRetryTestSTTSettings(t, result.stt.requests[0], settings)
}

func TestAutomaticWarningRetriesDoNotRetryTechnicalFailure(t *testing.T) {
	t.Parallel()

	result := runRetryTest(
		t,
		defaultGenerationSettings(),
		&retryTestTTS{err: errors.New("worker unavailable")},
		&retryTestSTT{},
	)

	if len(result.tts.requests) != 1 ||
		len(result.stt.requests) != 0 ||
		len(result.history) != 1 {
		t.Fatalf(
			"attempts = TTS:%d STT:%d history:%d, want 1/0/1",
			len(result.tts.requests),
			len(result.stt.requests),
			len(result.history),
		)
	}
	if result.job.Status != JobStatusFailed ||
		result.fragment.Status != FragmentStatusFailed ||
		result.fragment.Attempt != 1 {
		t.Fatalf(
			"final state = job:%+v fragment:%+v",
			result.job,
			result.fragment,
		)
	}
}

func assertRetryTestTTSSettings(
	t *testing.T,
	request TTSRequest,
	settings GenerationSettings,
) {
	t.Helper()

	got := OmniVoiceGenerationSettings{
		NumSteps:            request.NumSteps,
		GuidanceScale:       request.GuidanceScale,
		Speed:               request.Speed,
		NormalizeText:       request.NormalizeText,
		Denoise:             request.Denoise,
		TShift:              request.TShift,
		LayerPenaltyFactor:  request.LayerPenaltyFactor,
		PositionTemperature: request.PositionTemperature,
		ClassTemperature:    request.ClassTemperature,
		PreprocessPrompt:    request.PreprocessPrompt,
		PostprocessOutput:   request.PostprocessOutput,
		AudioChunkDuration:  request.AudioChunkDuration,
		AudioChunkThreshold: request.AudioChunkThreshold,
		PadDuration:         request.PadDuration,
		FadeDuration:        request.FadeDuration,
	}
	if got != settings.OmniVoice || !request.SettingsResolved {
		t.Errorf("TTS settings = %+v/resolved=%v, want %+v/true", got, request.SettingsResolved, settings.OmniVoice)
	}
}

func assertRetryTestSTTSettings(
	t *testing.T,
	request STTRequest,
	settings GenerationSettings,
) {
	t.Helper()

	got := WhisperGenerationSettings{
		BeamSize:       request.BeamSize,
		Patience:       request.Patience,
		Temperature:    request.Temperature,
		VADFilter:      request.VADFilter,
		WordTimestamps: request.WordTimestamps,
	}
	if !reflect.DeepEqual(got, settings.Whisper) ||
		!request.SettingsResolved ||
		request.Language != "ru" {
		t.Errorf(
			"STT settings = %+v/resolved=%v/language=%q, want %+v/true/ru",
			got,
			request.SettingsResolved,
			request.Language,
			settings.Whisper,
		)
	}
}
