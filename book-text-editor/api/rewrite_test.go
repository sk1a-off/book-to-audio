package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type endpointTestRewriter struct {
	mu       sync.Mutex
	models   RewriteModelsResponse
	requests []RewriterRequest
	rewrite  func(RewriterRequest) (RewriterResult, error)
}

func (f *endpointTestRewriter) Models(
	context.Context,
) (RewriteModelsResponse, error) {
	return f.models, nil
}

func (f *endpointTestRewriter) Rewrite(
	ctx context.Context,
	request RewriterRequest,
) (RewriterResult, error) {
	if err := ctx.Err(); err != nil {
		return RewriterResult{}, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	if f.rewrite != nil {
		return f.rewrite(request)
	}
	return RewriterResult{
		APIVersion:    "v1",
		RequestID:     request.RequestID,
		FragmentID:    request.FragmentID,
		RewrittenText: request.Text,
		Reason:        "Изменения не требуются.",
		ModelID:       request.ModelID,
		ModelRevision: "bc640142c66e1fdd12af0bd68f40445458f3869b",
		DurationMS:    1,
	}, nil
}

func (f *endpointTestRewriter) Requests() []RewriterRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func testRewriteModels() RewriteModelsResponse {
	return RewriteModelsResponse{
		Models: []RewriteModelResource{{
			ID:                "qwen3-4b-q4-k-m",
			DisplayName:       "Qwen3 4B",
			RepoID:            "Qwen/Qwen3-4B-GGUF",
			Revision:          "bc640142c66e1fdd12af0bd68f40445458f3869b",
			Filename:          "Qwen3-4B-Q4_K_M.gguf",
			SizeBytes:         2_497_280_256,
			SHA256:            "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5",
			Quantization:      "Q4_K_M",
			Recommended:       true,
			Available:         false,
			Loaded:            false,
			IntegrityVerified: false,
			Device:            "cpu",
		}},
		DefaultModelID: "qwen3-4b-q4-k-m",
		DefaultPrompt:  "Сохрани всю фразу.",
		Settings: RewriteSettings{
			Temperature: FloatSettingRange{
				Default: 0.1,
				Minimum: 0,
				Maximum: 1,
			},
			TopK: IntegerSettingRange{
				Default: 20,
				Minimum: 0,
				Maximum: 200,
			},
			TopP: FloatSettingRange{
				Default: 0.8,
				Minimum: 0,
				Maximum: 1,
			},
			MinP: FloatSettingRange{
				Default: 0,
				Minimum: 0,
				Maximum: 1,
			},
			RepeatPenalty: FloatSettingRange{
				Default: 1.05,
				Minimum: 0.5,
				Maximum: 2,
			},
			MaxTokens: IntegerSettingRange{
				Default: 1_024,
				Minimum: 16,
				Maximum: 2_048,
			},
		},
		Limits: RewriteLimits{
			MaxRequestBytes: 32 << 10,
			MaxTextChars:    8_000,
			MaxSTTTextChars: 8_000,
			MaxPromptChars:  2_000,
			MaxOutputChars:  12_000,
			MaxReasonChars:  1_000,
		},
	}
}

func TestRewriteWarningsCreatesImmutableAIRevision(t *testing.T) {
	fixture, handler, jobID, warning := endpointWarningFixture(t)
	rewriter := &endpointTestRewriter{
		models: testRewriteModels(),
		rewrite: func(request RewriterRequest) (RewriterResult, error) {
			return RewriterResult{
				APIVersion:    "v1",
				RequestID:     request.RequestID,
				FragmentID:    request.FragmentID,
				RewrittenText: "Первая часть!",
				Reason:        "Уточнена завершающая пауза.",
				ModelID:       request.ModelID,
				ModelRevision: testRewriteModels().Models[0].Revision,
				DurationMS:    27,
			}, nil
		},
	}
	fixture.server.rewriter = rewriter

	modelsResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/rewrite/models",
		nil,
		"",
	)
	if modelsResponse.Code != http.StatusOK {
		t.Fatalf(
			"GET models status = %d, body = %s",
			modelsResponse.Code,
			modelsResponse.Body,
		)
	}

	createResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/job/"+jobID+"/rewrite/warnings",
		strings.NewReader(`{
			"fragment_ids":["`+warning.ID+`"],
			"temperature":0.25,
			"top_k":12,
			"top_p":0.7,
			"min_p":0.05,
			"repeat_penalty":1.1,
			"max_tokens":320
		}`),
		"application/json",
	)
	if createResponse.Code != http.StatusAccepted {
		t.Fatalf(
			"POST rewrite status = %d, body = %s",
			createResponse.Code,
			createResponse.Body,
		)
	}
	var created RewriteTaskResponse
	endpointTestDecodeJSON(t, createResponse, &created)
	if created.ID == "" ||
		created.Temperature != 0.25 ||
		created.TopK != 12 ||
		created.TopP != 0.7 ||
		created.MinP != 0.05 ||
		created.RepeatPenalty != 1.1 ||
		created.MaxTokens != 320 ||
		len(created.Fragments) != 1 {
		t.Fatalf("created rewrite = %+v", created)
	}

	completed := waitForRewriteTask(t, handler, created.ID)
	if completed.Status != RewriteTaskStatusCompleted ||
		completed.FragmentsCompleted != 1 ||
		len(completed.Fragments) != 1 ||
		!completed.Fragments[0].Changed ||
		completed.Fragments[0].RevisionID == "" {
		t.Fatalf("completed rewrite = %+v", completed)
	}

	requests := rewriter.Requests()
	if len(requests) != 1 ||
		requests[0].Text != warning.Text ||
		requests[0].STTText != warning.STTText ||
		requests[0].Temperature == nil ||
		*requests[0].Temperature != 0.25 ||
		requests[0].TopK == nil ||
		*requests[0].TopK != 12 ||
		requests[0].TopP == nil ||
		*requests[0].TopP != 0.7 ||
		requests[0].MinP == nil ||
		*requests[0].MinP != 0.05 ||
		requests[0].RepeatPenalty == nil ||
		*requests[0].RepeatPenalty != 1.1 ||
		requests[0].MaxTokens == nil ||
		*requests[0].MaxTokens != 320 {
		t.Fatalf("worker requests = %+v", requests)
	}

	historyResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+warning.ID+"/revisions",
		nil,
		"",
	)
	var history FragmentRevisionsResponse
	endpointTestDecodeJSON(t, historyResponse, &history)
	if len(history.Revisions) != 2 ||
		history.Revisions[1].Source != FragmentTextRevisionSourceAI ||
		history.Revisions[1].Text != "Первая часть!" ||
		history.Revisions[1].ModelID == nil ||
		*history.Revisions[1].ModelID != rewriter.models.DefaultModelID ||
		history.Revisions[1].ModelRevision == nil ||
		*history.Revisions[1].ModelRevision !=
			rewriter.models.Models[0].Revision ||
		history.Revisions[1].Prompt == nil ||
		*history.Revisions[1].Prompt != rewriter.models.DefaultPrompt {
		t.Fatalf("revision history = %+v", history)
	}
}

func TestRewriteWarningsRejectsPhraseContentMutation(t *testing.T) {
	fixture, handler, jobID, warning := endpointWarningFixture(t)
	fixture.server.rewriter = &endpointTestRewriter{
		models: testRewriteModels(),
		rewrite: func(request RewriterRequest) (RewriterResult, error) {
			return RewriterResult{
				APIVersion:    "v1",
				RequestID:     request.RequestID,
				FragmentID:    request.FragmentID,
				RewrittenText: request.Text + " Новая мысль.",
				Reason:        "Добавлен текст.",
				ModelID:       request.ModelID,
				ModelRevision: testRewriteModels().Models[0].Revision,
				DurationMS:    2,
			}, nil
		},
	}

	createResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/job/"+jobID+"/rewrite/warnings",
		strings.NewReader(`{"fragment_ids":["`+warning.ID+`"]}`),
		"application/json",
	)
	var created RewriteTaskResponse
	endpointTestDecodeJSON(t, createResponse, &created)
	completed := waitForRewriteTask(t, handler, created.ID)
	if completed.Status != RewriteTaskStatusFailed ||
		completed.FragmentsFailed != 1 ||
		completed.Fragments[0].Error !=
			"local text model did not preserve the complete phrase" {
		t.Fatalf("failed rewrite = %+v", completed)
	}

	historyResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+warning.ID+"/revisions",
		nil,
		"",
	)
	var history FragmentRevisionsResponse
	endpointTestDecodeJSON(t, historyResponse, &history)
	if len(history.Revisions) != 1 ||
		history.Revisions[0].Text != warning.Text {
		t.Fatalf("mutating rewrite changed history: %+v", history)
	}
}

func TestRewriteWarningsValidatesModelAndInferenceSettings(t *testing.T) {
	fixture, handler, jobID, _ := endpointWarningFixture(t)
	fixture.server.rewriter = &endpointTestRewriter{
		models: testRewriteModels(),
	}

	for _, test := range []struct {
		name string
		body string
		code string
	}{
		{
			name: "model",
			body: `{"model_id":"not-allowlisted"}`,
			code: "MODEL_NOT_ALLOWED",
		},
		{
			name: "temperature",
			body: `{"temperature":1.01}`,
			code: "INVALID_REWRITE_TEMPERATURE",
		},
		{
			name: "top k below range",
			body: `{"top_k":-1}`,
			code: "INVALID_REWRITE_TOP_K",
		},
		{
			name: "top k above range",
			body: `{"top_k":201}`,
			code: "INVALID_REWRITE_TOP_K",
		},
		{
			name: "top p below range",
			body: `{"top_p":-0.01}`,
			code: "INVALID_REWRITE_TOP_P",
		},
		{
			name: "top p above range",
			body: `{"top_p":1.01}`,
			code: "INVALID_REWRITE_TOP_P",
		},
		{
			name: "min p below range",
			body: `{"min_p":-0.01}`,
			code: "INVALID_REWRITE_MIN_P",
		},
		{
			name: "min p above range",
			body: `{"min_p":1.01}`,
			code: "INVALID_REWRITE_MIN_P",
		},
		{
			name: "repeat penalty below range",
			body: `{"repeat_penalty":0.49}`,
			code: "INVALID_REWRITE_REPEAT_PENALTY",
		},
		{
			name: "repeat penalty above range",
			body: `{"repeat_penalty":2.01}`,
			code: "INVALID_REWRITE_REPEAT_PENALTY",
		},
		{
			name: "max tokens",
			body: `{"max_tokens":15}`,
			code: "INVALID_REWRITE_MAX_TOKENS",
		},
		{
			name: "max tokens above range",
			body: `{"max_tokens":2049}`,
			code: "INVALID_REWRITE_MAX_TOKENS",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := endpointTestRequest(
				t,
				handler,
				http.MethodPost,
				"/v1/job/"+jobID+"/rewrite/warnings",
				strings.NewReader(test.body),
				"application/json",
			)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf(
					"status = %d, body = %s",
					response.Code,
					response.Body,
				)
			}
			var problem ErrorResponse
			endpointTestDecodeJSON(t, response, &problem)
			if problem.Code != test.code {
				t.Fatalf("problem = %+v, want code %s", problem, test.code)
			}
		})
	}
}

func TestRewriteModelsUnavailableFailsClosed(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	response := endpointTestRequest(
		t,
		fixture.server.Handler(),
		http.MethodGet,
		"/v1/rewrite/models",
		nil,
		"",
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func waitForRewriteTask(
	t *testing.T,
	handler http.Handler,
	rewriteID string,
) RewriteTaskResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response := endpointTestRequest(
			t,
			handler,
			http.MethodGet,
			"/v1/rewrite/"+rewriteID,
			nil,
			"",
		)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"GET rewrite status = %d, body = %s",
				response.Code,
				response.Body,
			)
		}
		var task RewriteTaskResponse
		endpointTestDecodeJSON(t, response, &task)
		switch task.Status {
		case RewriteTaskStatusCompleted,
			RewriteTaskStatusCompletedWithErrors,
			RewriteTaskStatusFailed:
			return task
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(errors.New("timed out waiting for rewrite task"))
	return RewriteTaskResponse{}
}
