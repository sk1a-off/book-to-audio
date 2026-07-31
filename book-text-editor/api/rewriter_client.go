package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	maxRewriterResponseBodyBytes int64 = 1 << 20
	maxRewriterModelsBodyBytes   int64 = 256 << 10
)

// RewriterClient is the narrow, stateless boundary to the local text model.
// Persistent state and all fragment lifecycle decisions remain in Go.
type RewriterClient interface {
	Models(context.Context) (RewriteModelsResponse, error)
	Rewrite(context.Context, RewriterRequest) (RewriterResult, error)
}

type RewriteModelResource struct {
	ID                string `json:"id"`
	DisplayName       string `json:"display_name"`
	RepoID            string `json:"repo_id"`
	Revision          string `json:"revision"`
	Filename          string `json:"filename"`
	SizeBytes         int64  `json:"size_bytes"`
	SHA256            string `json:"sha256"`
	Quantization      string `json:"quantization"`
	Recommended       bool   `json:"recommended"`
	Available         bool   `json:"available"`
	Loaded            bool   `json:"loaded"`
	IntegrityVerified bool   `json:"integrity_verified"`
	Device            string `json:"device"`
}

type RewriteModelsResponse struct {
	Models         []RewriteModelResource `json:"models"`
	DefaultModelID string                 `json:"default_model_id"`
	DefaultPrompt  string                 `json:"default_prompt"`
	LoadedModelID  string                 `json:"loaded_model_id,omitempty"`
	Settings       RewriteSettings        `json:"settings"`
	Limits         RewriteLimits          `json:"limits"`
}

type FloatSettingRange struct {
	Default float64 `json:"default"`
	Minimum float64 `json:"minimum"`
	Maximum float64 `json:"maximum"`
}

type IntegerSettingRange struct {
	Default int `json:"default"`
	Minimum int `json:"minimum"`
	Maximum int `json:"maximum"`
}

type RewriteSettings struct {
	Temperature   FloatSettingRange   `json:"temperature"`
	TopK          IntegerSettingRange `json:"top_k"`
	TopP          FloatSettingRange   `json:"top_p"`
	MinP          FloatSettingRange   `json:"min_p"`
	RepeatPenalty FloatSettingRange   `json:"repeat_penalty"`
	MaxTokens     IntegerSettingRange `json:"max_tokens"`
}

type RewriteLimits struct {
	MaxRequestBytes int `json:"max_request_bytes"`
	MaxTextChars    int `json:"max_text_chars"`
	MaxSTTTextChars int `json:"max_stt_text_chars"`
	MaxPromptChars  int `json:"max_prompt_chars"`
	MaxOutputChars  int `json:"max_output_chars"`
	MaxReasonChars  int `json:"max_reason_chars"`
}

type RewriterRequest struct {
	APIVersion    string   `json:"api_version"`
	RequestID     string   `json:"request_id"`
	FragmentID    string   `json:"fragment_id"`
	Text          string   `json:"text"`
	STTText       string   `json:"stt_text,omitempty"`
	WarningCode   string   `json:"warning_code,omitempty"`
	ModelID       string   `json:"model_id"`
	Prompt        string   `json:"prompt"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	MinP          *float64 `json:"min_p,omitempty"`
	RepeatPenalty *float64 `json:"repeat_penalty,omitempty"`
	MaxTokens     *int     `json:"max_tokens,omitempty"`
}

type RewriterResult struct {
	APIVersion    string `json:"api_version"`
	RequestID     string `json:"request_id"`
	FragmentID    string `json:"fragment_id"`
	RewrittenText string `json:"rewritten_text"`
	Reason        string `json:"reason"`
	ModelID       string `json:"model_id"`
	ModelRevision string `json:"model_revision"`
	DurationMS    int64  `json:"duration_ms"`
}

type RewriterHTTPClient struct {
	modelsEndpoint  string
	rewriteEndpoint string
	client          *http.Client
}

func NewRewriterHTTPClient(
	baseURL string,
	client *http.Client,
) (*RewriterHTTPClient, error) {
	modelsEndpoint, err := buildWorkerEndpoint(baseURL, "/v1/models")
	if err != nil {
		return nil, fmt.Errorf("create rewriter client: %w", err)
	}
	rewriteEndpoint, err := buildWorkerEndpoint(baseURL, "/v1/rewrite")
	if err != nil {
		return nil, fmt.Errorf("create rewriter client: %w", err)
	}

	return &RewriterHTTPClient{
		modelsEndpoint:  modelsEndpoint,
		rewriteEndpoint: rewriteEndpoint,
		client:          workerHTTPClient(client),
	}, nil
}

func (c *RewriterHTTPClient) Models(
	ctx context.Context,
) (RewriteModelsResponse, error) {
	var result RewriteModelsResponse
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.modelsEndpoint,
		nil,
	)
	if err != nil {
		return result, fmt.Errorf("create rewriter models request: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := c.client.Do(request)
	if err != nil {
		return result, fmt.Errorf("call rewriter worker: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK ||
		response.StatusCode >= http.StatusMultipleChoices {
		return result, workerStatusError("rewriter", response)
	}

	body, err := readBoundedResponse(response, maxRewriterModelsBodyBytes)
	if err != nil {
		return result, fmt.Errorf("decode rewriter models response: %w", err)
	}
	var wire rewriteModelsWire
	if err := decodeWorkerJSON(body, &wire); err != nil {
		return result, fmt.Errorf("decode rewriter models response: %w", err)
	}
	if wire.APIVersion != "v1" || strings.TrimSpace(wire.WorkerID) == "" {
		return result, errors.New(
			"decode rewriter models response: invalid worker identity",
		)
	}
	result = mapRewriteModels(wire)
	if err := validateRewriteModels(result); err != nil {
		return RewriteModelsResponse{}, fmt.Errorf(
			"validate rewriter models response: %w",
			err,
		)
	}
	if result.Models == nil {
		result.Models = make([]RewriteModelResource, 0)
	}

	return result, nil
}

type rewriteModelWire struct {
	ModelID           string `json:"model_id"`
	DisplayName       string `json:"display_name"`
	RepoID            string `json:"repo_id"`
	Revision          string `json:"revision"`
	Filename          string `json:"filename"`
	SHA256            string `json:"sha256"`
	SizeBytes         int64  `json:"size_bytes"`
	Quantization      string `json:"quantization"`
	Recommended       bool   `json:"recommended"`
	Available         bool   `json:"available"`
	Loaded            bool   `json:"loaded"`
	IntegrityVerified bool   `json:"integrity_verified"`
	Device            string `json:"device"`
}

type rewriteModelsWire struct {
	APIVersion     string             `json:"api_version"`
	WorkerID       string             `json:"worker_id"`
	DefaultModelID string             `json:"default_model_id"`
	DefaultPrompt  string             `json:"default_prompt"`
	LoadedModelID  *string            `json:"loaded_model_id"`
	Settings       RewriteSettings    `json:"settings"`
	Limits         RewriteLimits      `json:"limits"`
	Models         []rewriteModelWire `json:"models"`
}

func mapRewriteModels(wire rewriteModelsWire) RewriteModelsResponse {
	result := RewriteModelsResponse{
		DefaultModelID: wire.DefaultModelID,
		DefaultPrompt:  wire.DefaultPrompt,
		Settings:       wire.Settings,
		Limits:         wire.Limits,
		Models:         make([]RewriteModelResource, 0, len(wire.Models)),
	}
	if wire.LoadedModelID != nil {
		result.LoadedModelID = *wire.LoadedModelID
	}
	for _, model := range wire.Models {
		result.Models = append(result.Models, RewriteModelResource{
			ID:                model.ModelID,
			DisplayName:       model.DisplayName,
			RepoID:            model.RepoID,
			Revision:          model.Revision,
			Filename:          model.Filename,
			SizeBytes:         model.SizeBytes,
			SHA256:            model.SHA256,
			Quantization:      model.Quantization,
			Recommended:       model.Recommended,
			Available:         model.Available,
			Loaded:            model.Loaded,
			IntegrityVerified: model.IntegrityVerified,
			Device:            model.Device,
		})
	}
	return result
}

func (c *RewriterHTTPClient) Rewrite(
	ctx context.Context,
	input RewriterRequest,
) (RewriterResult, error) {
	var result RewriterResult
	if input.APIVersion == "" {
		input.APIVersion = "v1"
	}
	body, err := json.Marshal(input)
	if err != nil {
		return result, fmt.Errorf("encode rewriter request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.rewriteEndpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return result, fmt.Errorf("create rewriter request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", input.RequestID)

	response, err := c.client.Do(request)
	if err != nil {
		return result, fmt.Errorf("call rewriter worker: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK ||
		response.StatusCode >= http.StatusMultipleChoices {
		return result, workerStatusError("rewriter", response)
	}

	responseBody, err := readBoundedResponse(
		response,
		maxRewriterResponseBodyBytes,
	)
	if err != nil {
		return result, fmt.Errorf("decode rewriter response: %w", err)
	}
	if err := decodeWorkerJSON(responseBody, &result); err != nil {
		return result, fmt.Errorf("decode rewriter response: %w", err)
	}
	if err := validateRewriterResult(input, result); err != nil {
		return RewriterResult{}, fmt.Errorf(
			"validate rewriter response: %w",
			err,
		)
	}

	return result, nil
}

func decodeWorkerJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("response must contain exactly one JSON object")
	}
	return nil
}

func validateRewriteModels(response RewriteModelsResponse) error {
	if len(response.Models) == 0 {
		return errors.New("worker returned no configured models")
	}
	seen := make(map[string]struct{}, len(response.Models))
	defaultFound := false
	for _, model := range response.Models {
		if strings.TrimSpace(model.ID) == "" ||
			strings.TrimSpace(model.DisplayName) == "" ||
			strings.TrimSpace(model.RepoID) == "" ||
			strings.TrimSpace(model.Revision) == "" ||
			strings.TrimSpace(model.Filename) == "" ||
			model.SizeBytes <= 0 ||
			len(model.SHA256) != 64 ||
			strings.TrimSpace(model.Quantization) == "" ||
			model.Device != "cpu" {
			return fmt.Errorf("invalid model capability %q", model.ID)
		}
		if _, duplicate := seen[model.ID]; duplicate {
			return fmt.Errorf("duplicate model id %q", model.ID)
		}
		seen[model.ID] = struct{}{}
		if model.ID == response.DefaultModelID {
			defaultFound = true
		}
	}
	if !defaultFound {
		return errors.New("default_model_id is not present in models")
	}
	if strings.TrimSpace(response.DefaultPrompt) == "" {
		return errors.New("default_prompt is empty")
	}
	if !validFloatSettingRange(
		response.Settings.Temperature,
		0,
		1,
	) {
		return errors.New("invalid temperature settings")
	}
	if !validIntegerSettingRange(
		response.Settings.MaxTokens,
		16,
		2_048,
	) {
		return errors.New("invalid max_tokens settings")
	}
	if !validIntegerSettingRange(response.Settings.TopK, 0, 200) {
		return errors.New("invalid top_k settings")
	}
	if !validFloatSettingRange(response.Settings.TopP, 0, 1) {
		return errors.New("invalid top_p settings")
	}
	if !validFloatSettingRange(response.Settings.MinP, 0, 1) {
		return errors.New("invalid min_p settings")
	}
	if !validFloatSettingRange(response.Settings.RepeatPenalty, 0.5, 2) {
		return errors.New("invalid repeat_penalty settings")
	}
	if response.Limits.MaxRequestBytes <= 0 ||
		response.Limits.MaxTextChars <= 0 ||
		response.Limits.MaxSTTTextChars <= 0 ||
		response.Limits.MaxPromptChars <= 0 ||
		response.Limits.MaxOutputChars <= 0 ||
		response.Limits.MaxReasonChars <= 0 {
		return errors.New("invalid rewriter limits")
	}
	return nil
}

func validFloatSettingRange(
	setting FloatSettingRange,
	minimum, maximum float64,
) bool {
	return finiteFloat(setting.Minimum) &&
		finiteFloat(setting.Default) &&
		finiteFloat(setting.Maximum) &&
		setting.Minimum >= minimum &&
		setting.Maximum <= maximum &&
		setting.Minimum <= setting.Default &&
		setting.Default <= setting.Maximum
}

func validIntegerSettingRange(
	setting IntegerSettingRange,
	minimum, maximum int,
) bool {
	return setting.Minimum >= minimum &&
		setting.Maximum <= maximum &&
		setting.Minimum <= setting.Default &&
		setting.Default <= setting.Maximum
}

func validateRewriterResult(
	request RewriterRequest,
	result RewriterResult,
) error {
	if result.APIVersion != "v1" {
		return fmt.Errorf("unsupported api_version %q", result.APIVersion)
	}
	if result.RequestID != request.RequestID {
		return fmt.Errorf(
			"request id %q does not match %q",
			result.RequestID,
			request.RequestID,
		)
	}
	if result.FragmentID != request.FragmentID {
		return fmt.Errorf(
			"fragment id %q does not match %q",
			result.FragmentID,
			request.FragmentID,
		)
	}
	if strings.TrimSpace(result.RewrittenText) == "" {
		return errors.New("rewritten_text is empty")
	}
	if strings.TrimSpace(result.Reason) == "" {
		return errors.New("reason is empty")
	}
	if result.ModelID != request.ModelID ||
		strings.TrimSpace(result.ModelRevision) == "" {
		return errors.New("model identity is missing or does not match request")
	}
	if result.DurationMS < 0 {
		return errors.New("duration_ms must not be negative")
	}
	return nil
}

var _ RewriterClient = (*RewriterHTTPClient)(nil)
