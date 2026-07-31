package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRewriterHTTPClientModelsAndRewrite(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch {
			case request.Method == http.MethodGet &&
				request.URL.Path == "/worker/v1/models":
				_, _ = io.WriteString(writer, `{
					"models":[{
						"model_id":"qwen3-4b-q4-k-m",
						"display_name":"Qwen3 4B Q4_K_M",
						"repo_id":"Qwen/Qwen3-4B-GGUF",
						"revision":"bc640142c66e1fdd12af0bd68f40445458f3869b",
						"filename":"Qwen3-4B-Q4_K_M.gguf",
						"size_bytes":2497280256,
						"sha256":"7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5",
						"quantization":"Q4_K_M",
						"recommended":true,
						"available":false,
						"loaded":false,
						"integrity_verified":false,
						"device":"cpu"
					}],
					"api_version":"v1",
					"worker_id":"rewriter-1",
					"default_model_id":"qwen3-4b-q4-k-m",
					"default_prompt":"Сохрани смысл.",
					"loaded_model_id":null,
					"settings":{
						"temperature":{"default":0.1,"minimum":0,"maximum":1},
						"top_k":{"default":20,"minimum":0,"maximum":200},
						"top_p":{"default":0.8,"minimum":0,"maximum":1},
						"min_p":{"default":0,"minimum":0,"maximum":1},
						"repeat_penalty":{"default":1.05,"minimum":0.5,"maximum":2},
						"max_tokens":{"default":1024,"minimum":16,"maximum":2048}
					},
					"limits":{
						"max_request_bytes":32768,
						"max_text_chars":8000,
						"max_stt_text_chars":8000,
						"max_prompt_chars":2000,
						"max_output_chars":12000,
						"max_reason_chars":1000
					}
				}`)

			case request.Method == http.MethodPost &&
				request.URL.Path == "/worker/v1/rewrite":
				if request.Header.Get("Content-Type") != "application/json" {
					t.Errorf(
						"Content-Type = %q, want application/json",
						request.Header.Get("Content-Type"),
					)
				}
				var input RewriterRequest
				if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
					t.Errorf("decode request: %v", err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if input.APIVersion != "v1" ||
					input.RequestID != "request-1" ||
					input.FragmentID != "fragment-1" ||
					input.ModelID != "qwen3-4b-q4-k-m" ||
					input.Text != "В 2026 г." ||
					input.STTText != "В две тысячи двадцать шестом" ||
					input.WarningCode != "transcript_mismatch" ||
					input.Temperature == nil ||
					*input.Temperature != 0.2 ||
					input.TopK == nil ||
					*input.TopK != 10 ||
					input.TopP == nil ||
					*input.TopP != 0.7 ||
					input.MinP == nil ||
					*input.MinP != 0.05 ||
					input.RepeatPenalty == nil ||
					*input.RepeatPenalty != 1.1 ||
					input.MaxTokens == nil ||
					*input.MaxTokens != 512 {
					t.Errorf("unexpected rewrite request: %+v", input)
				}
				_ = json.NewEncoder(writer).Encode(RewriterResult{
					APIVersion:    "v1",
					RequestID:     input.RequestID,
					FragmentID:    input.FragmentID,
					RewrittenText: "В две тысячи двадцать шестом году.",
					Reason:        "Число и сокращение раскрыты.",
					ModelID:       input.ModelID,
					ModelRevision: "bc640142c66e1fdd12af0bd68f40445458f3869b",
					DurationMS:    1250,
				})

			default:
				http.NotFound(writer, request)
			}
		},
	))
	defer server.Close()

	client, err := NewRewriterHTTPClient(server.URL+"/worker/", server.Client())
	if err != nil {
		t.Fatalf("NewRewriterHTTPClient() error = %v", err)
	}
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if models.DefaultModelID != "qwen3-4b-q4-k-m" ||
		len(models.Models) != 1 ||
		!models.Models[0].Recommended ||
		models.Settings.Temperature.Default != 0.1 ||
		models.Settings.MaxTokens.Maximum != 2048 ||
		models.Limits.MaxPromptChars != 2000 {
		t.Fatalf("Models() = %+v", models)
	}

	temperature := 0.2
	topK := 10
	topP := 0.7
	minP := 0.05
	repeatPenalty := 1.1
	maxTokens := 512
	result, err := client.Rewrite(context.Background(), RewriterRequest{
		RequestID:     "request-1",
		FragmentID:    "fragment-1",
		Text:          "В 2026 г.",
		STTText:       "В две тысячи двадцать шестом",
		WarningCode:   "transcript_mismatch",
		ModelID:       models.DefaultModelID,
		Prompt:        models.DefaultPrompt,
		Temperature:   &temperature,
		TopK:          &topK,
		TopP:          &topP,
		MinP:          &minP,
		RepeatPenalty: &repeatPenalty,
		MaxTokens:     &maxTokens,
	})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}
	if result.RewrittenText != "В две тысячи двадцать шестом году." ||
		result.ModelRevision == "" ||
		result.DurationMS != 1250 {
		t.Fatalf("Rewrite() = %+v", result)
	}
}

func TestRewriterHTTPClientRejectsInvalidResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "mismatched request",
			body: `{
				"api_version":"v1",
				"request_id":"another",
				"fragment_id":"fragment-1",
				"rewritten_text":"Текст.",
				"reason":"Причина.",
				"model_id":"model",
				"model_revision":"revision",
				"duration_ms":1
			}`,
		},
		{
			name: "unknown field",
			body: `{
				"api_version":"v1",
				"request_id":"request",
				"fragment_id":"fragment-1",
				"rewritten_text":"Текст.",
				"reason":"Причина.",
				"model_id":"model",
				"model_revision":"revision",
				"duration_ms":1,
				"unexpected":true
			}`,
		},
		{
			name: "empty text",
			body: `{
				"api_version":"v1",
				"request_id":"request",
				"fragment_id":"fragment-1",
				"rewritten_text":" ",
				"reason":"Причина.",
				"model_id":"model",
				"model_revision":"revision",
				"duration_ms":1
			}`,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(
				func(writer http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(writer, test.body)
				},
			))
			defer server.Close()

			client, err := NewRewriterHTTPClient(server.URL, server.Client())
			if err != nil {
				t.Fatalf("create client: %v", err)
			}
			_, err = client.Rewrite(context.Background(), RewriterRequest{
				RequestID:  "request",
				FragmentID: "fragment-1",
				ModelID:    "model",
			})
			if err == nil {
				t.Fatal("Rewrite() error = nil, want invalid response")
			}
		})
	}
}

func TestValidateRewriteModelsRejectsOutOfServerBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*RewriteSettings)
	}{
		{
			name: "temperature is not finite",
			mutate: func(settings *RewriteSettings) {
				settings.Temperature.Default = math.NaN()
			},
		},
		{
			name: "top k exceeds store contract",
			mutate: func(settings *RewriteSettings) {
				settings.TopK.Maximum = 201
			},
		},
		{
			name: "top p below store contract",
			mutate: func(settings *RewriteSettings) {
				settings.TopP.Minimum = -0.01
			},
		},
		{
			name: "min p exceeds store contract",
			mutate: func(settings *RewriteSettings) {
				settings.MinP.Maximum = 1.01
			},
		},
		{
			name: "repeat penalty below store contract",
			mutate: func(settings *RewriteSettings) {
				settings.RepeatPenalty.Minimum = 0.49
			},
		},
		{
			name: "max tokens exceeds store contract",
			mutate: func(settings *RewriteSettings) {
				settings.MaxTokens.Maximum = 2_049
			},
		},
		{
			name: "default is outside advertised range",
			mutate: func(settings *RewriteSettings) {
				settings.TopP.Default = settings.TopP.Maximum + 0.01
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			models := testRewriteModels()
			test.mutate(&models.Settings)
			if err := validateRewriteModels(models); err == nil {
				t.Fatal("validateRewriteModels() error = nil")
			}
		})
	}
}

func TestRewriterHTTPClientBoundsStatusAndSuccessBodies(t *testing.T) {
	t.Parallel()

	t.Run("typed status error", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(
					writer,
					strings.Repeat("x", int(maxWorkerErrorBodyBytes)+128),
				)
			},
		))
		defer server.Close()

		client, err := NewRewriterHTTPClient(server.URL, server.Client())
		if err != nil {
			t.Fatalf("create client: %v", err)
		}
		_, err = client.Models(context.Background())
		var statusError *WorkerHTTPError
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("Models() error = %v, want 503", err)
		}
		if !errors.As(err, &statusError) ||
			!statusError.BodyTruncated {
			t.Fatalf("Models() error = %#v, want bounded typed error", err)
		}
	})

	t.Run("oversized success", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set(
					"Content-Length",
					"1048577",
				)
				writer.WriteHeader(http.StatusOK)
			},
		))
		defer server.Close()

		client, err := NewRewriterHTTPClient(server.URL, server.Client())
		if err != nil {
			t.Fatalf("create client: %v", err)
		}
		_, err = client.Rewrite(context.Background(), RewriterRequest{})
		if err == nil || !strings.Contains(err.Error(), ErrWorkerResponseTooLarge.Error()) {
			t.Fatalf("Rewrite() error = %v, want response limit", err)
		}
	})
}
