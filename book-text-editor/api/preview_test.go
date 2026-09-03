package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQualityPreviewUsesResolvedSettingsAndSelectivePronunciation(t *testing.T) {
	t.Parallel()

	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	_, voice := endpointTestUploadInputs(t, handler)
	body := `{
		"text":"Старинный замок стоял на вершине холма и был закрыт на дверной замок.",
		"seed":17,
		"generation_settings":{
			"omnivoice":{"num_steps":24,"speed":0.95},
			"russian_text":{
				"selective_stress":false,
				"normalize_morphology":false
			},
			"pronunciation":{
				"enabled":true,
				"rules":"старинный замок => старинный за́мок\nвершине холма => вершине холма́\nдверной замок => дверной замо́к"
			}
		}
	}`
	response := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/preview/voice/"+voice.ID+"/audio.wav",
		strings.NewReader(body),
		"application/json",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", response.Code, response.Body)
	}
	if response.Header().Get("Content-Type") != "audio/wav" ||
		response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-TTS-Seed") != "17" ||
		response.Header().Get("X-Pronunciation-Rules-Applied") != "3" ||
		response.Header().Get("X-Selective-Stress-Rules-Applied") != "0" ||
		response.Header().Get("X-Morphology-Replacements") != "0" ||
		response.Header().Get("X-Russian-Text-Version") != "ru-selective-morph-v3" ||
		response.Header().Get("X-Audio-Duration-Ms") != "1" {
		t.Fatalf("preview headers = %#v", response.Header())
	}
	wav := response.Body.Bytes()
	if len(wav) <= 44 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("preview WAV is invalid: %x", wav)
	}
	requests := fixture.tts.Requests()
	if len(requests) != 1 {
		t.Fatalf("TTS requests=%d, want 1", len(requests))
	}
	request := requests[0]
	if request.Text != "Старинный за́мок стоял на вершине холма́ и был закрыт на дверной замо́к." ||
		request.Seed == nil || *request.Seed != 17 ||
		request.NumSteps != 24 || request.Speed != 0.95 ||
		request.JobID != "preview" || !request.SettingsResolved {
		t.Fatalf("preview TTS request = %+v", request)
	}
	if len(fixture.stt.Requests()) != 0 {
		t.Fatal("quality preview must not invoke Whisper")
	}
}

func TestQualityPreviewRejectsInvalidInputsBeforeTTS(t *testing.T) {
	t.Parallel()

	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	_, voice := endpointTestUploadInputs(t, handler)
	tests := []struct {
		name string
		path string
		body string
		code int
	}{
		{
			name: "short text",
			path: "/v1/preview/voice/" + voice.ID + "/audio.wav",
			body: `{"text":"слишком коротко"}`,
			code: http.StatusUnprocessableEntity,
		},
		{
			name: "unsafe rule",
			path: "/v1/preview/voice/" + voice.ID + "/audio.wav",
			body: `{"text":"Достаточно длинный тестовый фрагмент для preview.","generation_settings":{"pronunciation":{"enabled":true,"rules":"замок => дворе́ц"}}}`,
			code: http.StatusUnprocessableEntity,
		},
		{
			name: "missing voice",
			path: "/v1/preview/voice/missing/audio.wav",
			body: `{"text":"Достаточно длинный тестовый фрагмент для preview."}`,
			code: http.StatusNotFound,
		},
		{
			name: "unknown field",
			path: "/v1/preview/voice/" + voice.ID + "/audio.wav",
			body: `{"text":"Достаточно длинный тестовый фрагмент для preview.","unknown":true}`,
			code: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := endpointTestRequest(
				t,
				handler,
				http.MethodPost,
				test.path,
				strings.NewReader(test.body),
				"application/json",
			)
			if response.Code != test.code {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body, test.code)
			}
		})
	}
	if len(fixture.tts.Requests()) != 0 {
		t.Fatal("invalid previews must not invoke TTS")
	}
}

func TestQualityPreviewCanApplyBuiltInRussianPreparation(t *testing.T) {
	t.Parallel()

	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	_, voice := endpointTestUploadInputs(t, handler)
	response := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/preview/voice/"+voice.ID+"/audio.wav",
		strings.NewReader(`{
			"text":"Глава 7. Старинный замок стоял на вершине холма.",
			"generation_settings":{"russian_text":{
				"selective_stress":true,
				"normalize_morphology":true
			}}
		}`),
		"application/json",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", response.Code, response.Body)
	}
	if response.Header().Get("X-Pronunciation-Rules-Applied") != "0" ||
		response.Header().Get("X-Selective-Stress-Rules-Applied") != "2" ||
		response.Header().Get("X-Morphology-Replacements") != "1" {
		t.Fatalf("preview preparation headers = %#v", response.Header())
	}
	requests := fixture.tts.Requests()
	if len(requests) != 1 ||
		requests[0].Text != "Глава седьмая. Старинный за́мок стоял на вершине холма́." {
		t.Fatalf("preview TTS requests = %+v", requests)
	}
}

type blockingPreviewTTS struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *blockingPreviewTTS) Generate(ctx context.Context, _ TTSRequest) (TTSResult, error) {
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
		return TTSResult{
			AudioPCM:    []byte{0, 0, 1, 0},
			SampleRate:  24_000,
			Channels:    1,
			SampleWidth: 2,
			DurationMS:  1,
		}, nil
	case <-ctx.Done():
		return TTSResult{}, ctx.Err()
	}
}

func TestQualityPreviewRejectsConcurrentRequest(t *testing.T) {
	t.Parallel()

	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	_, voice := endpointTestUploadInputs(t, handler)
	client := &blockingPreviewTTS{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	fixture.server.tts = client
	body := `{"text":"Достаточно длинный тестовый фрагмент для preview."}`
	firstRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/preview/voice/"+voice.ID+"/audio.wav",
		strings.NewReader(body),
	)
	firstRequest.Header.Set("Content-Type", "application/json")
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("first preview did not reach TTS")
	}

	secondResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/preview/voice/"+voice.ID+"/audio.wav",
		strings.NewReader(body),
		"application/json",
	)
	if secondResponse.Code != http.StatusTooManyRequests ||
		secondResponse.Header().Get("Retry-After") != "5" {
		t.Fatalf("second preview status=%d headers=%v body=%s", secondResponse.Code, secondResponse.Header(), secondResponse.Body)
	}
	close(client.release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first preview did not finish")
	}
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first preview status=%d body=%s", firstResponse.Code, firstResponse.Body)
	}
}

func TestGenerateTTSWaitForSharedSlotIsContextCancellable(t *testing.T) {
	t.Parallel()

	client := &blockingPreviewTTS{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	server := &Server{tts: client, ttsSlot: make(chan struct{}, 1)}
	firstDone := make(chan error, 1)
	go func() {
		_, err := server.generateTTS(context.Background(), TTSRequest{})
		firstDone <- err
	}()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("first TTS call did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := server.generateTTS(ctx, TTSRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second TTS error=%v, want deadline exceeded", err)
	}
	close(client.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first TTS error=%v", err)
	}
}
