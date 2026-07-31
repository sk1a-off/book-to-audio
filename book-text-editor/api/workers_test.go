package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOmniVoiceHTTPClientGenerate(t *testing.T) {
	t.Parallel()

	referenceAudio := []byte("reference-audio")
	pcm := []byte{1, 0, 2, 0}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", request.Method)
		}
		if request.URL.Path != "/worker/v1/generate" {
			t.Errorf("path = %q, want /worker/v1/generate", request.URL.Path)
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		var metadata map[string]any
		if err := json.Unmarshal([]byte(request.FormValue("metadata")), &metadata); err != nil {
			t.Errorf("decode metadata: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		assertJSONField(t, metadata, "api_version", "v1")
		assertJSONField(t, metadata, "request_id", "request-1")
		assertJSONField(t, metadata, "job_id", "job-1")
		assertJSONField(t, metadata, "segment_id", "segment-1")
		assertJSONField(t, metadata, "text", "Тестовый текст.")
		assertJSONField(t, metadata, "language", "ru")
		assertJSONField(t, metadata, "mode", "VOICE_CLONE")
		assertJSONField(t, metadata, "voice_reference_text", "Эталонный текст.")
		assertJSONField(t, metadata, "num_steps", float64(defaultTTSNumSteps))
		assertJSONField(
			t,
			metadata,
			"guidance_scale",
			defaultTTSGuidanceScale,
		)
		assertJSONField(t, metadata, "speed", defaultTTSSpeed)
		assertJSONField(t, metadata, "normalize_text", true)
		assertJSONField(t, metadata, "denoise", defaultTTSDenoise)
		assertJSONField(t, metadata, "t_shift", defaultTTSTShift)
		assertJSONField(
			t,
			metadata,
			"layer_penalty_factor",
			defaultTTSLayerPenaltyFactor,
		)
		assertJSONField(
			t,
			metadata,
			"position_temperature",
			defaultTTSPositionTemperature,
		)
		assertJSONField(
			t,
			metadata,
			"class_temperature",
			defaultTTSClassTemperature,
		)
		assertJSONField(
			t,
			metadata,
			"preprocess_prompt",
			defaultTTSPreprocessPrompt,
		)
		assertJSONField(
			t,
			metadata,
			"postprocess_output",
			defaultTTSPostprocessOutput,
		)
		assertJSONField(
			t,
			metadata,
			"audio_chunk_duration",
			defaultTTSAudioChunkDuration,
		)
		assertJSONField(
			t,
			metadata,
			"audio_chunk_threshold",
			defaultTTSAudioChunkThreshold,
		)
		assertJSONField(t, metadata, "pad_duration", defaultTTSPadDuration)
		assertJSONField(t, metadata, "fade_duration", defaultTTSFadeDuration)
		if _, exists := metadata["seed"]; exists {
			t.Errorf("default metadata contains optional seed: %#v", metadata["seed"])
		}
		assertJSONField(t, metadata, "output_encoding", "PCM_S16LE")
		referenceHash := sha256.Sum256(referenceAudio)
		assertJSONField(
			t,
			metadata,
			"voice_reference_sha256",
			hex.EncodeToString(referenceHash[:]),
		)

		file, header, err := request.FormFile("voice_reference")
		if err != nil {
			t.Errorf("read voice_reference: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		if header.Header.Get("Content-Type") != "audio/flac" {
			t.Errorf("content type = %q, want audio/flac", header.Header.Get("Content-Type"))
		}
		gotAudio, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("read reference audio: %v", err)
		}
		if string(gotAudio) != string(referenceAudio) {
			t.Errorf("reference audio = %q, want %q", gotAudio, referenceAudio)
		}

		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("X-Request-ID", "request-1")
		writer.Header().Set("X-Audio-Sample-Rate", "24000")
		writer.Header().Set("X-Audio-Channels", "1")
		writer.Header().Set("X-Audio-Bits-Per-Sample", "16")
		writer.Header().Set("X-Audio-Duration-Ms", "125")
		writer.Header().Set("X-Audio-Warnings", "FIRST, SECOND")
		_, _ = writer.Write(pcm)
	}))
	defer server.Close()

	client, err := NewOmniVoiceHTTPClient(server.URL+"/worker/", server.Client())
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	result, err := client.Generate(context.Background(), TTSRequest{
		RequestID:            "request-1",
		JobID:                "job-1",
		FragmentID:           "segment-1",
		Text:                 "Тестовый текст.",
		ReferenceAudio:       referenceAudio,
		ReferenceContentType: "audio/flac",
		ReferenceText:        "Эталонный текст.",
	})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if result.RequestID != "request-1" {
		t.Errorf("RequestID = %q, want request-1", result.RequestID)
	}
	if string(result.AudioPCM) != string(pcm) {
		t.Errorf("AudioPCM = %v, want %v", result.AudioPCM, pcm)
	}
	if result.SampleRate != 24_000 {
		t.Errorf("SampleRate = %d, want 24000", result.SampleRate)
	}
	if result.Channels != 1 || result.SampleWidth != 2 || result.DurationMS != 125 {
		t.Errorf("unexpected audio metadata: %+v", result)
	}
	if len(result.Warnings) != 2 ||
		result.Warnings[0] != "FIRST" ||
		result.Warnings[1] != "SECOND" {
		t.Errorf("Warnings = %#v, want [FIRST SECOND]", result.Warnings)
	}
}

func TestBuildTTSRequestBodyGenerationSettings(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()

		metadata := decodeTTSRequestMetadata(t, TTSRequest{})
		assertJSONField(t, metadata, "num_steps", float64(32))
		assertJSONField(t, metadata, "guidance_scale", 2.0)
		assertJSONField(t, metadata, "speed", 1.0)
		assertJSONField(t, metadata, "normalize_text", true)
		assertJSONField(t, metadata, "denoise", true)
		assertJSONField(t, metadata, "t_shift", 0.1)
		assertJSONField(t, metadata, "layer_penalty_factor", 5.0)
		assertJSONField(t, metadata, "position_temperature", 5.0)
		assertJSONField(t, metadata, "class_temperature", 0.0)
		assertJSONField(t, metadata, "preprocess_prompt", true)
		assertJSONField(t, metadata, "postprocess_output", true)
		assertJSONField(t, metadata, "audio_chunk_duration", 15.0)
		assertJSONField(t, metadata, "audio_chunk_threshold", 30.0)
		assertJSONField(t, metadata, "pad_duration", 0.1)
		assertJSONField(t, metadata, "fade_duration", 0.1)
		if _, exists := metadata["seed"]; exists {
			t.Errorf("default metadata contains optional seed: %#v", metadata["seed"])
		}
	})

	t.Run("custom", func(t *testing.T) {
		t.Parallel()

		seed := uint32(42)
		metadata := decodeTTSRequestMetadata(t, TTSRequest{
			NumSteps:            8,
			GuidanceScale:       3.5,
			Speed:               1.25,
			NormalizeText:       false,
			Denoise:             false,
			TShift:              0.2,
			LayerPenaltyFactor:  4.0,
			PositionTemperature: 3.0,
			ClassTemperature:    0.4,
			PreprocessPrompt:    false,
			PostprocessOutput:   false,
			AudioChunkDuration:  12.0,
			AudioChunkThreshold: 24.0,
			PadDuration:         0.0,
			FadeDuration:        0.05,
			Seed:                &seed,
			SettingsResolved:    true,
		})
		assertJSONField(t, metadata, "num_steps", float64(8))
		assertJSONField(t, metadata, "guidance_scale", 3.5)
		assertJSONField(t, metadata, "speed", 1.25)
		assertJSONField(t, metadata, "normalize_text", false)
		assertJSONField(t, metadata, "denoise", false)
		assertJSONField(t, metadata, "t_shift", 0.2)
		assertJSONField(t, metadata, "layer_penalty_factor", 4.0)
		assertJSONField(t, metadata, "position_temperature", 3.0)
		assertJSONField(t, metadata, "class_temperature", 0.4)
		assertJSONField(t, metadata, "preprocess_prompt", false)
		assertJSONField(t, metadata, "postprocess_output", false)
		assertJSONField(t, metadata, "audio_chunk_duration", 12.0)
		assertJSONField(t, metadata, "audio_chunk_threshold", 24.0)
		assertJSONField(t, metadata, "pad_duration", 0.0)
		assertJSONField(t, metadata, "fade_duration", 0.05)
		assertJSONField(t, metadata, "seed", float64(42))

		for _, invalidKey := range []string{
			"numSteps",
			"guidanceScale",
			"generation_speed",
		} {
			if _, exists := metadata[invalidKey]; exists {
				t.Errorf("metadata contains non-contract field %q", invalidKey)
			}
		}
	})

	t.Run("explicit zero seed", func(t *testing.T) {
		t.Parallel()

		seed := uint32(0)
		metadata := decodeTTSRequestMetadata(t, TTSRequest{Seed: &seed})
		assertJSONField(t, metadata, "seed", float64(0))
	})

	t.Run("explicit zero guidance", func(t *testing.T) {
		t.Parallel()

		metadata := decodeTTSRequestMetadata(t, TTSRequest{
			NumSteps:            8,
			GuidanceScale:       0,
			Speed:               1,
			NormalizeText:       true,
			Denoise:             true,
			TShift:              defaultTTSTShift,
			LayerPenaltyFactor:  defaultTTSLayerPenaltyFactor,
			PositionTemperature: defaultTTSPositionTemperature,
			ClassTemperature:    defaultTTSClassTemperature,
			PreprocessPrompt:    true,
			PostprocessOutput:   true,
			AudioChunkDuration:  defaultTTSAudioChunkDuration,
			AudioChunkThreshold: defaultTTSAudioChunkThreshold,
			PadDuration:         defaultTTSPadDuration,
			FadeDuration:        defaultTTSFadeDuration,
			SettingsResolved:    true,
		})
		assertJSONField(t, metadata, "guidance_scale", 0.0)
	})
}

func TestBuildTTSRequestBodyRejectsInvalidGenerationSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		request   TTSRequest
		wantField string
	}{
		{
			name:      "negative num steps",
			request:   TTSRequest{NumSteps: -1},
			wantField: "num_steps",
		},
		{
			name:      "too many num steps",
			request:   TTSRequest{NumSteps: 101},
			wantField: "num_steps",
		},
		{
			name:      "negative guidance",
			request:   TTSRequest{GuidanceScale: -0.01},
			wantField: "guidance_scale",
		},
		{
			name:      "excessive guidance",
			request:   TTSRequest{GuidanceScale: 10.01},
			wantField: "guidance_scale",
		},
		{
			name:      "NaN guidance",
			request:   TTSRequest{GuidanceScale: math.NaN()},
			wantField: "guidance_scale",
		},
		{
			name:      "infinite guidance",
			request:   TTSRequest{GuidanceScale: math.Inf(1)},
			wantField: "guidance_scale",
		},
		{
			name:      "slow speed",
			request:   TTSRequest{Speed: 0.49},
			wantField: "speed",
		},
		{
			name:      "fast speed",
			request:   TTSRequest{Speed: 2.01},
			wantField: "speed",
		},
		{
			name:      "NaN speed",
			request:   TTSRequest{Speed: math.NaN()},
			wantField: "speed",
		},
		{
			name:      "infinite speed",
			request:   TTSRequest{Speed: math.Inf(-1)},
			wantField: "speed",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := buildTTSRequestBody(test.request)
			if err == nil || !strings.Contains(err.Error(), test.wantField) {
				t.Fatalf(
					"buildTTSRequestBody() error = %v, want %q validation error",
					err,
					test.wantField,
				)
			}
		})
	}
}

func TestBuildTTSRequestBodyRejectsInvalidAdvancedGenerationSettings(
	t *testing.T,
) {
	t.Parallel()

	tests := []struct {
		name      string
		wantField string
		mutate    func(*TTSRequest)
	}{
		{
			name:      "t shift below range",
			wantField: "t_shift",
			mutate: func(request *TTSRequest) {
				request.TShift = 0
			},
		},
		{
			name:      "t shift is infinite",
			wantField: "t_shift",
			mutate: func(request *TTSRequest) {
				request.TShift = math.Inf(1)
			},
		},
		{
			name:      "layer penalty above range",
			wantField: "layer_penalty_factor",
			mutate: func(request *TTSRequest) {
				request.LayerPenaltyFactor = 20.01
			},
		},
		{
			name:      "position temperature above range",
			wantField: "position_temperature",
			mutate: func(request *TTSRequest) {
				request.PositionTemperature = 20.01
			},
		},
		{
			name:      "class temperature below range",
			wantField: "class_temperature",
			mutate: func(request *TTSRequest) {
				request.ClassTemperature = -0.01
			},
		},
		{
			name:      "audio chunk duration below range",
			wantField: "audio_chunk_duration",
			mutate: func(request *TTSRequest) {
				request.AudioChunkDuration = 0.99
			},
		},
		{
			name:      "audio chunk threshold above range",
			wantField: "audio_chunk_threshold",
			mutate: func(request *TTSRequest) {
				request.AudioChunkThreshold = 600.01
			},
		},
		{
			name:      "pad duration is NaN",
			wantField: "pad_duration",
			mutate: func(request *TTSRequest) {
				request.PadDuration = math.NaN()
			},
		},
		{
			name:      "fade duration above range",
			wantField: "fade_duration",
			mutate: func(request *TTSRequest) {
				request.FadeDuration = 5.01
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request := resolvedTTSRequestForTest()
			test.mutate(&request)
			_, _, err := buildTTSRequestBody(request)
			if err == nil || !strings.Contains(err.Error(), test.wantField) {
				t.Fatalf(
					"buildTTSRequestBody() error = %v, want %q validation error",
					err,
					test.wantField,
				)
			}
		})
	}
}

func resolvedTTSRequestForTest() TTSRequest {
	settings := defaultGenerationSettings().OmniVoice
	return TTSRequest{
		NumSteps:            settings.NumSteps,
		GuidanceScale:       settings.GuidanceScale,
		Speed:               settings.Speed,
		NormalizeText:       settings.NormalizeText,
		Denoise:             settings.Denoise,
		TShift:              settings.TShift,
		LayerPenaltyFactor:  settings.LayerPenaltyFactor,
		PositionTemperature: settings.PositionTemperature,
		ClassTemperature:    settings.ClassTemperature,
		PreprocessPrompt:    settings.PreprocessPrompt,
		PostprocessOutput:   settings.PostprocessOutput,
		AudioChunkDuration:  settings.AudioChunkDuration,
		AudioChunkThreshold: settings.AudioChunkThreshold,
		PadDuration:         settings.PadDuration,
		FadeDuration:        settings.FadeDuration,
		SettingsResolved:    true,
	}
}

func TestBuildSTTRequestBodyGenerationSettings(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()

		metadata := decodeSTTRequestMetadata(t, STTRequest{})
		assertJSONField(t, metadata, "beam_size", float64(5))
		assertJSONField(t, metadata, "patience", 1.0)
		assertJSONField(t, metadata, "temperature", 0.0)
		assertJSONField(t, metadata, "vad_filter", false)
		assertJSONField(t, metadata, "word_timestamps_required", true)
	})

	t.Run("custom including false", func(t *testing.T) {
		t.Parallel()

		metadata := decodeSTTRequestMetadata(t, STTRequest{
			BeamSize:         7,
			Patience:         1.5,
			Temperature:      0.25,
			VADFilter:        true,
			WordTimestamps:   false,
			SettingsResolved: true,
		})
		assertJSONField(t, metadata, "beam_size", float64(7))
		assertJSONField(t, metadata, "patience", 1.5)
		assertJSONField(t, metadata, "temperature", 0.25)
		assertJSONField(t, metadata, "vad_filter", true)
		assertJSONField(t, metadata, "word_timestamps_required", false)
	})
}

func TestBuildSTTRequestBodyRejectsInvalidGenerationSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		request   STTRequest
		wantField string
	}{
		{"beam below range", STTRequest{BeamSize: -1}, "beam_size"},
		{"beam above range", STTRequest{BeamSize: 11}, "beam_size"},
		{"patience below range", STTRequest{Patience: 0.09}, "patience"},
		{"patience above range", STTRequest{Patience: 2.01}, "patience"},
		{"patience NaN", STTRequest{Patience: math.NaN()}, "patience"},
		{"temperature below range", STTRequest{Temperature: -0.01}, "temperature"},
		{"temperature above range", STTRequest{Temperature: 1.01}, "temperature"},
		{"temperature infinite", STTRequest{Temperature: math.Inf(1)}, "temperature"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := buildSTTRequestBody(test.request)
			if err == nil || !strings.Contains(err.Error(), test.wantField) {
				t.Fatalf(
					"buildSTTRequestBody() error = %v, want %q validation error",
					err,
					test.wantField,
				)
			}
		})
	}
}

func TestSTTHTTPClientTranscribe(t *testing.T) {
	t.Parallel()

	pcm := []byte{1, 0, 2, 0}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", request.Method)
		}
		if request.URL.Path != "/v1/transcribe" {
			t.Errorf("path = %q, want /v1/transcribe", request.URL.Path)
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		var metadata struct {
			APIVersion   string `json:"api_version"`
			RequestID    string `json:"request_id"`
			JobID        string `json:"job_id"`
			SegmentID    string `json:"segment_id"`
			ExpectedText string `json:"expected_text"`
			Language     string `json:"language"`
			AudioSHA256  string `json:"audio_sha256"`
			Audio        struct {
				Encoding      string `json:"encoding"`
				SampleRateHz  int    `json:"sample_rate_hz"`
				Channels      int    `json:"channels"`
				BitsPerSample int    `json:"bits_per_sample"`
			} `json:"audio"`
			BeamSize               int     `json:"beam_size"`
			Patience               float64 `json:"patience"`
			Temperature            float64 `json:"temperature"`
			VADFilter              bool    `json:"vad_filter"`
			WordTimestampsRequired bool    `json:"word_timestamps_required"`
		}
		if err := json.Unmarshal([]byte(request.FormValue("metadata")), &metadata); err != nil {
			t.Errorf("decode metadata: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if metadata.APIVersion != "v1" ||
			metadata.RequestID != "request-2" ||
			metadata.JobID != "job-2" ||
			metadata.SegmentID != "segment-2" {
			t.Errorf("unexpected correlation metadata: %+v", metadata)
		}
		if metadata.ExpectedText != "Ожидаемый текст." || metadata.Language != "ru" {
			t.Errorf("unexpected text metadata: %+v", metadata)
		}
		if metadata.Audio.Encoding != "PCM_S16LE" ||
			metadata.Audio.SampleRateHz != 24_000 ||
			metadata.Audio.Channels != 1 ||
			metadata.Audio.BitsPerSample != 16 {
			t.Errorf("unexpected audio metadata: %+v", metadata.Audio)
		}
		pcmHash := sha256.Sum256(pcm)
		if metadata.AudioSHA256 != hex.EncodeToString(pcmHash[:]) {
			t.Errorf("audio_sha256 = %q, want %q", metadata.AudioSHA256, hex.EncodeToString(pcmHash[:]))
		}
		if !metadata.WordTimestampsRequired {
			t.Error("word_timestamps_required = false, want true")
		}
		if metadata.BeamSize != 5 ||
			metadata.Patience != 1 ||
			metadata.Temperature != 0 ||
			metadata.VADFilter {
			t.Errorf("unexpected STT inference settings: %+v", metadata)
		}

		file, header, err := request.FormFile("audio")
		if err != nil {
			t.Errorf("read audio: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		if header.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf(
				"content type = %q, want application/octet-stream",
				header.Header.Get("Content-Type"),
			)
		}
		gotPCM, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("read PCM: %v", err)
		}
		if string(gotPCM) != string(pcm) {
			t.Errorf("PCM = %v, want %v", gotPCM, pcm)
		}

		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
			"request_id":"request-2",
			"transcript":"Распознанный текст.",
			"detected_language":"ru",
			"duration_ms":125
		}`)
	}))
	defer server.Close()

	client, err := NewSTTHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	result, err := client.Transcribe(context.Background(), STTRequest{
		RequestID:    "request-2",
		JobID:        "job-2",
		FragmentID:   "segment-2",
		AudioPCM:     pcm,
		SampleRate:   24_000,
		Channels:     1,
		SampleWidth:  2,
		Language:     "ru",
		ExpectedText: "Ожидаемый текст.",
	})
	if err != nil {
		t.Fatalf("Transcribe() error: %v", err)
	}
	if result.RequestID != "request-2" {
		t.Errorf("RequestID = %q", result.RequestID)
	}
	if result.Text != "Распознанный текст." {
		t.Errorf("Text = %q", result.Text)
	}
	if result.Language != "ru" {
		t.Errorf("Language = %q", result.Language)
	}
	if result.DurationMS != 125 {
		t.Errorf("DurationMS = %d, want 125", result.DurationMS)
	}
}

func TestWorkerHTTPClientReturnsBoundedTypedStatusError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, strings.Repeat("x", int(maxWorkerErrorBodyBytes)+128))
	}))
	defer server.Close()

	client, err := NewSTTHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	_, err = client.Transcribe(context.Background(), STTRequest{
		RequestID:   "request",
		AudioPCM:    []byte{0, 0},
		SampleRate:  24_000,
		Channels:    1,
		SampleWidth: 2,
	})
	if err == nil {
		t.Fatal("Transcribe() error = nil, want status error")
	}
	var statusError *WorkerHTTPError
	if !errors.As(err, &statusError) {
		t.Fatalf("error type = %T, want *WorkerHTTPError", err)
	}
	if statusError.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", statusError.StatusCode)
	}
	if int64(len(statusError.Body)) > maxWorkerErrorBodyBytes {
		t.Errorf("error body has %d bytes, limit is %d", len(statusError.Body), maxWorkerErrorBodyBytes)
	}
	if !statusError.BodyTruncated {
		t.Error("BodyTruncated = false, want true")
	}
}

func TestOmniVoiceHTTPClientRequiresBoundedResponseAndSampleRate(t *testing.T) {
	t.Parallel()

	t.Run("missing sample rate", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte{0, 0})
		}))
		defer server.Close()

		client, err := NewOmniVoiceHTTPClient(server.URL, server.Client())
		if err != nil {
			t.Fatalf("create client: %v", err)
		}
		_, err = client.Generate(context.Background(), TTSRequest{})
		if err == nil || !strings.Contains(err.Error(), "X-Audio-Sample-Rate") {
			t.Fatalf("Generate() error = %v, want missing sample rate error", err)
		}
	})

	t.Run("oversized response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("X-Audio-Sample-Rate", "24000")
			writer.Header().Set("X-Audio-Channels", "1")
			writer.Header().Set("X-Audio-Bits-Per-Sample", "16")
			writer.Header().Set("X-Audio-Duration-Ms", "0")
			writer.Header().Set(
				"Content-Length",
				strconv.FormatInt(maxTTSResponseBodyBytes+1, 10),
			)
			writer.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client, err := NewOmniVoiceHTTPClient(server.URL, server.Client())
		if err != nil {
			t.Fatalf("create client: %v", err)
		}
		_, err = client.Generate(context.Background(), TTSRequest{})
		if !errors.Is(err, ErrWorkerResponseTooLarge) {
			t.Fatalf("Generate() error = %v, want ErrWorkerResponseTooLarge", err)
		}
	})
}

func TestWorkerHTTPClientHonorsContext(t *testing.T) {
	t.Parallel()

	client, err := NewSTTHTTPClient("http://worker.invalid", nil)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = client.Transcribe(ctx, STTRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Transcribe() error = %v, want context.Canceled", err)
	}
}

func TestWorkerHTTPClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var redirectFollowed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirected" {
			redirectFollowed.Store(true)
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(writer, request, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := NewSTTHTTPClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	_, err = client.Transcribe(context.Background(), STTRequest{})
	var statusError *WorkerHTTPError
	if !errors.As(err, &statusError) {
		t.Fatalf("Transcribe() error = %v, want *WorkerHTTPError", err)
	}
	if statusError.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("StatusCode = %d, want 307", statusError.StatusCode)
	}
	if redirectFollowed.Load() {
		t.Error("worker client followed redirect")
	}
}

func TestWorkerHTTPClientsRejectMismatchedCorrelationIDs(t *testing.T) {
	t.Parallel()

	t.Run("OmniVoice request id", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("X-Request-ID", "another-request")
				writer.Header().Set("X-Audio-Sample-Rate", "24000")
				writer.Header().Set("X-Audio-Channels", "1")
				writer.Header().Set("X-Audio-Bits-Per-Sample", "16")
				writer.Header().Set("X-Audio-Duration-Ms", "1")
				_, _ = writer.Write([]byte{0, 0})
			},
		))
		defer server.Close()

		client, err := NewOmniVoiceHTTPClient(server.URL, server.Client())
		if err != nil {
			t.Fatalf("create client: %v", err)
		}
		_, err = client.Generate(context.Background(), TTSRequest{
			RequestID: "expected-request",
		})
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("Generate() error = %v, want correlation error", err)
		}
	})

	t.Run("STT segment id", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(
					writer,
					`{"request_id":"request","segment_id":"other","transcript":"text"}`,
				)
			},
		))
		defer server.Close()

		client, err := NewSTTHTTPClient(server.URL, server.Client())
		if err != nil {
			t.Fatalf("create client: %v", err)
		}
		_, err = client.Transcribe(context.Background(), STTRequest{
			RequestID:   "request",
			FragmentID:  "expected",
			AudioPCM:    []byte{0, 0},
			SampleRate:  24_000,
			Channels:    1,
			SampleWidth: 2,
		})
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("Transcribe() error = %v, want correlation error", err)
		}
	})
}

func TestWorkerHTTPClientRejectsInvalidBaseURL(t *testing.T) {
	t.Parallel()

	for _, baseURL := range []string{
		"",
		"worker:8001",
		"ftp://worker.example",
		"https://user:password@worker.example",
		"https://worker.example?token=secret",
	} {
		baseURL := baseURL
		t.Run(baseURL, func(t *testing.T) {
			t.Parallel()
			if _, err := NewOmniVoiceHTTPClient(baseURL, nil); err == nil {
				t.Fatalf("NewOmniVoiceHTTPClient(%q) error = nil", baseURL)
			}
			if _, err := NewSTTHTTPClient(baseURL, nil); err == nil {
				t.Fatalf("NewSTTHTTPClient(%q) error = nil", baseURL)
			}
		})
	}
}

func assertJSONField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	if got := object[key]; got != want {
		t.Errorf("%s = %#v, want %#v", key, got, want)
	}
}

func decodeTTSRequestMetadata(
	t *testing.T,
	request TTSRequest,
) map[string]any {
	t.Helper()

	body, contentType, err := buildTTSRequestBody(request)
	if err != nil {
		t.Fatalf("buildTTSRequestBody() error = %v", err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/v1/generate", body)
	httpRequest.Header.Set("Content-Type", contentType)
	if err := httpRequest.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("parse generated multipart body: %v", err)
	}

	var metadata map[string]any
	if err := json.Unmarshal(
		[]byte(httpRequest.FormValue("metadata")),
		&metadata,
	); err != nil {
		t.Fatalf("decode generated metadata: %v", err)
	}
	return metadata
}

func decodeSTTRequestMetadata(
	t *testing.T,
	request STTRequest,
) map[string]any {
	t.Helper()

	body, contentType, err := buildSTTRequestBody(request)
	if err != nil {
		t.Fatalf("buildSTTRequestBody() error = %v", err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/v1/transcribe", body)
	httpRequest.Header.Set("Content-Type", contentType)
	if err := httpRequest.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("parse generated multipart body: %v", err)
	}

	var metadata map[string]any
	if err := json.Unmarshal(
		[]byte(httpRequest.FormValue("metadata")),
		&metadata,
	); err != nil {
		t.Fatalf("decode generated metadata: %v", err)
	}
	return metadata
}
