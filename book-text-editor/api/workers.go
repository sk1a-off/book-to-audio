package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

const (
	maxTTSResponseBodyBytes int64 = 64 << 20
	maxSTTResponseBodyBytes int64 = 8 << 20
	maxWorkerErrorBodyBytes int64 = 64 << 10

	defaultTTSNumSteps      = 32
	defaultTTSGuidanceScale = 2.0
	defaultTTSSpeed         = 1.0
)

var ErrWorkerResponseTooLarge = errors.New("worker response exceeds the configured limit")

type TTSClient interface {
	Generate(context.Context, TTSRequest) (TTSResult, error)
}

type STTClient interface {
	Transcribe(context.Context, STTRequest) (STTResult, error)
}

type TTSRequest struct {
	RequestID            string
	JobID                string
	FragmentID           string
	Text                 string
	ReferenceAudio       []byte
	ReferenceContentType string
	ReferenceText        string
	NumSteps             int
	GuidanceScale        float64
	Speed                float64
	NormalizeText        bool
	Denoise              bool
	TShift               float64
	LayerPenaltyFactor   float64
	PositionTemperature  float64
	ClassTemperature     float64
	PreprocessPrompt     bool
	PostprocessOutput    bool
	AudioChunkDuration   float64
	AudioChunkThreshold  float64
	PadDuration          float64
	FadeDuration         float64
	Seed                 *uint32
	SettingsResolved     bool
}

type TTSResult struct {
	RequestID            string
	AudioPCM             []byte
	SampleRate           int
	Channels             int
	SampleWidth          int
	DurationMS           int
	GenerationDurationMS int
	SeedUsed             *uint32
	VoiceCacheHit        *bool
	ModelID              string
	ModelVersion         string
	AudioSHA256          string
	Warnings             []string
}

type STTRequest struct {
	RequestID        string
	JobID            string
	FragmentID       string
	AudioPCM         []byte
	SampleRate       int
	Channels         int
	SampleWidth      int
	Language         string
	ExpectedText     string
	BeamSize         int
	Patience         float64
	Temperature      float64
	VADFilter        bool
	WordTimestamps   bool
	SettingsResolved bool
}

type STTResult struct {
	RequestID  string
	Text       string
	Language   string
	DurationMS int
}

// WorkerHTTPError represents a valid HTTP response with a non-2xx status.
// Body is always bounded by maxWorkerErrorBodyBytes.
type WorkerHTTPError struct {
	Worker        string
	StatusCode    int
	Body          string
	BodyTruncated bool
}

func (e *WorkerHTTPError) Error() string {
	message := fmt.Sprintf("%s worker returned HTTP status %d", e.Worker, e.StatusCode)
	if e.Body != "" {
		message += ": " + strconv.Quote(e.Body)
	}
	if e.BodyTruncated {
		message += " (response body truncated)"
	}
	return message
}

type OmniVoiceHTTPClient struct {
	endpoint string
	client   *http.Client
}

func NewOmniVoiceHTTPClient(baseURL string, client *http.Client) (*OmniVoiceHTTPClient, error) {
	endpoint, err := buildWorkerEndpoint(baseURL, "/v1/generate")
	if err != nil {
		return nil, fmt.Errorf("create OmniVoice client: %w", err)
	}
	return &OmniVoiceHTTPClient{
		endpoint: endpoint,
		client:   workerHTTPClient(client),
	}, nil
}

func (c *OmniVoiceHTTPClient) Generate(
	ctx context.Context,
	request TTSRequest,
) (TTSResult, error) {
	var result TTSResult

	body, contentType, err := buildTTSRequestBody(request)
	if err != nil {
		return result, fmt.Errorf("encode OmniVoice request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, body)
	if err != nil {
		return result, fmt.Errorf("create OmniVoice request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.Header.Set("Accept", "application/octet-stream")

	response, err := c.client.Do(httpRequest)
	if err != nil {
		return result, fmt.Errorf("call OmniVoice worker: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return result, workerStatusError("OmniVoice", response)
	}

	audioMetadata, err := parseTTSAudioMetadata(response.Header)
	if err != nil {
		return result, fmt.Errorf("decode OmniVoice response: %w", err)
	}
	pcm, err := readBoundedResponse(response, maxTTSResponseBodyBytes)
	if err != nil {
		return result, fmt.Errorf("decode OmniVoice response: %w", err)
	}

	requestID := response.Header.Get("X-Request-ID")
	if requestID != "" && requestID != request.RequestID {
		return result, fmt.Errorf(
			"decode OmniVoice response: request id %q does not match %q",
			requestID,
			request.RequestID,
		)
	}
	requestID = request.RequestID
	frameSize := audioMetadata.channels * audioMetadata.sampleWidth
	if len(pcm) == 0 || frameSize <= 0 || len(pcm)%frameSize != 0 {
		return result, errors.New(
			"decode OmniVoice response: PCM payload is empty or not frame-aligned",
		)
	}
	audioSHA256 := strings.ToLower(strings.TrimSpace(response.Header.Get("X-Audio-SHA256")))
	if audioSHA256 != "" {
		digest := sha256.Sum256(pcm)
		actual := hex.EncodeToString(digest[:])
		if audioSHA256 != actual {
			return result, fmt.Errorf(
				"decode OmniVoice response: X-Audio-SHA256 %q does not match payload %q",
				audioSHA256,
				actual,
			)
		}
	}
	generationDurationMS, err := parseOptionalNonNegativeHeader(
		response.Header,
		"X-Generation-Duration-Ms",
	)
	if err != nil {
		return result, fmt.Errorf("decode OmniVoice response: %w", err)
	}
	seedUsed, err := parseOptionalUint32Header(response.Header, "X-Seed-Used")
	if err != nil {
		return result, fmt.Errorf("decode OmniVoice response: %w", err)
	}
	voiceCacheHit, err := parseOptionalBoolHeader(response.Header, "X-Voice-Cache-Hit")
	if err != nil {
		return result, fmt.Errorf("decode OmniVoice response: %w", err)
	}
	return TTSResult{
		RequestID:            requestID,
		AudioPCM:             pcm,
		SampleRate:           audioMetadata.sampleRate,
		Channels:             audioMetadata.channels,
		SampleWidth:          audioMetadata.sampleWidth,
		DurationMS:           audioMetadata.durationMS,
		GenerationDurationMS: generationDurationMS,
		SeedUsed:             seedUsed,
		VoiceCacheHit:        voiceCacheHit,
		ModelID:              strings.TrimSpace(response.Header.Get("X-Model-ID")),
		ModelVersion:         strings.TrimSpace(response.Header.Get("X-Model-Version")),
		AudioSHA256:          audioSHA256,
		Warnings:             audioMetadata.warnings,
	}, nil
}

type STTHTTPClient struct {
	endpoint string
	client   *http.Client
}

func NewSTTHTTPClient(baseURL string, client *http.Client) (*STTHTTPClient, error) {
	endpoint, err := buildWorkerEndpoint(baseURL, "/v1/transcribe")
	if err != nil {
		return nil, fmt.Errorf("create STT client: %w", err)
	}
	return &STTHTTPClient{
		endpoint: endpoint,
		client:   workerHTTPClient(client),
	}, nil
}

func (c *STTHTTPClient) Transcribe(
	ctx context.Context,
	request STTRequest,
) (STTResult, error) {
	var result STTResult

	body, contentType, err := buildSTTRequestBody(request)
	if err != nil {
		return result, fmt.Errorf("encode STT request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, body)
	if err != nil {
		return result, fmt.Errorf("create STT request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.Header.Set("Accept", "application/json")

	response, err := c.client.Do(httpRequest)
	if err != nil {
		return result, fmt.Errorf("call STT worker: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return result, workerStatusError("STT", response)
	}
	responseBody, err := readBoundedResponse(response, maxSTTResponseBodyBytes)
	if err != nil {
		return result, fmt.Errorf("decode STT response: %w", err)
	}

	var wireResponse struct {
		RequestID        string `json:"request_id"`
		SegmentID        string `json:"segment_id"`
		Transcript       string `json:"transcript"`
		DetectedLanguage string `json:"detected_language"`
		DurationMS       int    `json:"duration_ms"`
	}
	if err := json.Unmarshal(responseBody, &wireResponse); err != nil {
		return result, fmt.Errorf("decode STT response: %w", err)
	}

	if wireResponse.RequestID != "" &&
		wireResponse.RequestID != request.RequestID {
		return result, fmt.Errorf(
			"decode STT response: request id %q does not match %q",
			wireResponse.RequestID,
			request.RequestID,
		)
	}
	if wireResponse.SegmentID != "" &&
		wireResponse.SegmentID != request.FragmentID {
		return result, fmt.Errorf(
			"decode STT response: segment id %q does not match %q",
			wireResponse.SegmentID,
			request.FragmentID,
		)
	}
	return STTResult{
		RequestID:  request.RequestID,
		Text:       wireResponse.Transcript,
		Language:   wireResponse.DetectedLanguage,
		DurationMS: wireResponse.DurationMS,
	}, nil
}

type ttsWorkerMetadata struct {
	APIVersion           string  `json:"api_version"`
	RequestID            string  `json:"request_id"`
	JobID                string  `json:"job_id,omitempty"`
	SegmentID            string  `json:"segment_id,omitempty"`
	Text                 string  `json:"text"`
	Language             string  `json:"language,omitempty"`
	Mode                 string  `json:"mode"`
	VoiceReferenceText   string  `json:"voice_reference_text,omitempty"`
	VoiceReferenceSHA256 string  `json:"voice_reference_sha256"`
	NumSteps             int     `json:"num_steps"`
	GuidanceScale        float64 `json:"guidance_scale"`
	Speed                float64 `json:"speed"`
	NormalizeText        bool    `json:"normalize_text"`
	Denoise              bool    `json:"denoise"`
	TShift               float64 `json:"t_shift"`
	LayerPenaltyFactor   float64 `json:"layer_penalty_factor"`
	PositionTemperature  float64 `json:"position_temperature"`
	ClassTemperature     float64 `json:"class_temperature"`
	PreprocessPrompt     bool    `json:"preprocess_prompt"`
	PostprocessOutput    bool    `json:"postprocess_output"`
	AudioChunkDuration   float64 `json:"audio_chunk_duration"`
	AudioChunkThreshold  float64 `json:"audio_chunk_threshold"`
	PadDuration          float64 `json:"pad_duration"`
	FadeDuration         float64 `json:"fade_duration"`
	Seed                 *uint32 `json:"seed,omitempty"`
	OutputEncoding       string  `json:"output_encoding"`
}

type sttWorkerMetadata struct {
	APIVersion             string             `json:"api_version"`
	RequestID              string             `json:"request_id"`
	JobID                  string             `json:"job_id,omitempty"`
	SegmentID              string             `json:"segment_id,omitempty"`
	ExpectedText           string             `json:"expected_text,omitempty"`
	Language               string             `json:"language,omitempty"`
	Audio                  sttWorkerAudioSpec `json:"audio"`
	AudioSHA256            string             `json:"audio_sha256"`
	BeamSize               int                `json:"beam_size"`
	Patience               float64            `json:"patience"`
	Temperature            float64            `json:"temperature"`
	VADFilter              bool               `json:"vad_filter"`
	WordTimestampsRequired bool               `json:"word_timestamps_required"`
}

type sttWorkerAudioSpec struct {
	Encoding      string `json:"encoding"`
	SampleRateHz  int    `json:"sample_rate_hz"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
}

func buildTTSRequestBody(request TTSRequest) (*bytes.Reader, string, error) {
	settings, err := validateTTSGenerationSettings(request)
	if err != nil {
		return nil, "", err
	}

	audioHash := sha256.Sum256(request.ReferenceAudio)
	metadata := ttsWorkerMetadata{
		APIVersion:           "v1",
		RequestID:            request.RequestID,
		JobID:                request.JobID,
		SegmentID:            request.FragmentID,
		Text:                 request.Text,
		Language:             "ru",
		Mode:                 "VOICE_CLONE",
		VoiceReferenceText:   request.ReferenceText,
		VoiceReferenceSHA256: hex.EncodeToString(audioHash[:]),
		NumSteps:             settings.numSteps,
		GuidanceScale:        settings.guidanceScale,
		Speed:                settings.speed,
		NormalizeText:        settings.normalizeText,
		Denoise:              settings.denoise,
		TShift:               settings.tShift,
		LayerPenaltyFactor:   settings.layerPenaltyFactor,
		PositionTemperature:  settings.positionTemperature,
		ClassTemperature:     settings.classTemperature,
		PreprocessPrompt:     settings.preprocessPrompt,
		PostprocessOutput:    settings.postprocessOutput,
		AudioChunkDuration:   settings.audioChunkDuration,
		AudioChunkThreshold:  settings.audioChunkThreshold,
		PadDuration:          settings.padDuration,
		FadeDuration:         settings.fadeDuration,
		Seed:                 request.Seed,
		OutputEncoding:       "PCM_S16LE",
	}

	contentType, err := normalizeMediaType(request.ReferenceContentType)
	if err != nil {
		return nil, "", err
	}
	return buildMultipartBody(
		metadata,
		"voice_reference",
		"voice-reference",
		contentType,
		request.ReferenceAudio,
	)
}

type ttsGenerationSettings struct {
	numSteps            int
	guidanceScale       float64
	speed               float64
	normalizeText       bool
	denoise             bool
	tShift              float64
	layerPenaltyFactor  float64
	positionTemperature float64
	classTemperature    float64
	preprocessPrompt    bool
	postprocessOutput   bool
	audioChunkDuration  float64
	audioChunkThreshold float64
	padDuration         float64
	fadeDuration        float64
}

func validateTTSGenerationSettings(
	request TTSRequest,
) (ttsGenerationSettings, error) {
	settings := ttsGenerationSettings{
		numSteps:            request.NumSteps,
		guidanceScale:       request.GuidanceScale,
		speed:               request.Speed,
		normalizeText:       request.NormalizeText,
		denoise:             request.Denoise,
		tShift:              request.TShift,
		layerPenaltyFactor:  request.LayerPenaltyFactor,
		positionTemperature: request.PositionTemperature,
		classTemperature:    request.ClassTemperature,
		preprocessPrompt:    request.PreprocessPrompt,
		postprocessOutput:   request.PostprocessOutput,
		audioChunkDuration:  request.AudioChunkDuration,
		audioChunkThreshold: request.AudioChunkThreshold,
		padDuration:         request.PadDuration,
		fadeDuration:        request.FadeDuration,
	}
	if !request.SettingsResolved {
		if settings.numSteps == 0 {
			settings.numSteps = defaultTTSNumSteps
		}
		if settings.guidanceScale == 0 {
			settings.guidanceScale = defaultTTSGuidanceScale
		}
		if settings.speed == 0 {
			settings.speed = defaultTTSSpeed
		}
		settings.normalizeText = true
		settings.denoise = defaultTTSDenoise
		settings.tShift = defaultTTSTShift
		settings.layerPenaltyFactor = defaultTTSLayerPenaltyFactor
		settings.positionTemperature = defaultTTSPositionTemperature
		settings.classTemperature = defaultTTSClassTemperature
		settings.preprocessPrompt = defaultTTSPreprocessPrompt
		settings.postprocessOutput = defaultTTSPostprocessOutput
		settings.audioChunkDuration = defaultTTSAudioChunkDuration
		settings.audioChunkThreshold = defaultTTSAudioChunkThreshold
		settings.padDuration = defaultTTSPadDuration
		settings.fadeDuration = defaultTTSFadeDuration
	}

	if settings.numSteps < 1 || settings.numSteps > 100 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"num_steps must be between 1 and 100",
		)
	}
	if !finiteFloat(settings.guidanceScale) ||
		settings.guidanceScale < 0 ||
		settings.guidanceScale > 10 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"guidance_scale must be between 0 and 10",
		)
	}
	if !finiteFloat(settings.speed) ||
		settings.speed < 0.5 ||
		settings.speed > 2 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"speed must be between 0.5 and 2",
		)
	}
	if !finiteFloat(settings.tShift) ||
		settings.tShift < 0.001 ||
		settings.tShift > 10 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"t_shift must be between 0.001 and 10",
		)
	}
	if !finiteFloat(settings.layerPenaltyFactor) ||
		settings.layerPenaltyFactor < 0 ||
		settings.layerPenaltyFactor > 20 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"layer_penalty_factor must be between 0 and 20",
		)
	}
	if !finiteFloat(settings.positionTemperature) ||
		settings.positionTemperature < 0 ||
		settings.positionTemperature > 20 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"position_temperature must be between 0 and 20",
		)
	}
	if !finiteFloat(settings.classTemperature) ||
		settings.classTemperature < 0 ||
		settings.classTemperature > 10 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"class_temperature must be between 0 and 10",
		)
	}
	if !finiteFloat(settings.audioChunkDuration) ||
		settings.audioChunkDuration < 1 ||
		settings.audioChunkDuration > 120 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"audio_chunk_duration must be between 1 and 120",
		)
	}
	if !finiteFloat(settings.audioChunkThreshold) ||
		settings.audioChunkThreshold < 1 ||
		settings.audioChunkThreshold > 600 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"audio_chunk_threshold must be between 1 and 600",
		)
	}
	if !finiteFloat(settings.padDuration) ||
		settings.padDuration < 0 ||
		settings.padDuration > 5 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"pad_duration must be between 0 and 5",
		)
	}
	if !finiteFloat(settings.fadeDuration) ||
		settings.fadeDuration < 0 ||
		settings.fadeDuration > 5 {
		return ttsGenerationSettings{}, fmt.Errorf(
			"fade_duration must be between 0 and 5",
		)
	}

	return settings, nil
}

func finiteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func buildSTTRequestBody(request STTRequest) (*bytes.Reader, string, error) {
	settings, err := validateSTTGenerationSettings(request)
	if err != nil {
		return nil, "", err
	}

	audioHash := sha256.Sum256(request.AudioPCM)
	language := request.Language
	if language == "" {
		language = "ru"
	}
	metadata := sttWorkerMetadata{
		APIVersion:   "v1",
		RequestID:    request.RequestID,
		JobID:        request.JobID,
		SegmentID:    request.FragmentID,
		ExpectedText: request.ExpectedText,
		Language:     language,
		Audio: sttWorkerAudioSpec{
			Encoding:      "PCM_S16LE",
			SampleRateHz:  request.SampleRate,
			Channels:      request.Channels,
			BitsPerSample: request.SampleWidth * 8,
		},
		AudioSHA256:            hex.EncodeToString(audioHash[:]),
		BeamSize:               settings.beamSize,
		Patience:               settings.patience,
		Temperature:            settings.temperature,
		VADFilter:              settings.vadFilter,
		WordTimestampsRequired: settings.wordTimestamps,
	}
	return buildMultipartBody(
		metadata,
		"audio",
		"segment.pcm",
		"application/octet-stream",
		request.AudioPCM,
	)
}

type sttGenerationSettings struct {
	beamSize       int
	patience       float64
	temperature    float64
	vadFilter      bool
	wordTimestamps bool
}

func validateSTTGenerationSettings(
	request STTRequest,
) (sttGenerationSettings, error) {
	settings := sttGenerationSettings{
		beamSize:       request.BeamSize,
		patience:       request.Patience,
		temperature:    request.Temperature,
		vadFilter:      request.VADFilter,
		wordTimestamps: request.WordTimestamps,
	}
	if !request.SettingsResolved {
		if settings.beamSize == 0 {
			settings.beamSize = defaultWhisperBeamSize
		}
		if settings.patience == 0 {
			settings.patience = defaultWhisperPatience
		}
		settings.wordTimestamps = defaultWhisperWordTimestamps
	}

	if settings.beamSize < 1 || settings.beamSize > 10 {
		return sttGenerationSettings{}, fmt.Errorf(
			"beam_size must be between 1 and 10",
		)
	}
	if !finiteFloat(settings.patience) ||
		settings.patience < 0.1 ||
		settings.patience > 2 {
		return sttGenerationSettings{}, fmt.Errorf(
			"patience must be between 0.1 and 2",
		)
	}
	if !finiteFloat(settings.temperature) ||
		settings.temperature < 0 ||
		settings.temperature > 1 {
		return sttGenerationSettings{}, fmt.Errorf(
			"temperature must be between 0 and 1",
		)
	}
	return settings, nil
}

func buildMultipartBody(
	metadata any,
	fileField string,
	filename string,
	contentType string,
	payload []byte,
) (*bytes.Reader, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	metadataPart, err := writer.CreateFormField("metadata")
	if err != nil {
		return nil, "", err
	}
	if err := json.NewEncoder(metadataPart).Encode(metadata); err != nil {
		return nil, "", err
	}

	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Disposition", mime.FormatMediaType(
		"form-data",
		map[string]string{"name": fileField, "filename": filename},
	))
	headers.Set("Content-Type", contentType)
	filePart, err := writer.CreatePart(headers)
	if err != nil {
		return nil, "", err
	}
	if _, err := filePart.Write(payload); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}

	return bytes.NewReader(body.Bytes()), writer.FormDataContentType(), nil
}

func normalizeMediaType(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "application/octet-stream", nil
	}
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil {
		return "", fmt.Errorf("invalid reference content type: %w", err)
	}
	return mime.FormatMediaType(mediaType, parameters), nil
}

func buildWorkerEndpoint(baseURL string, endpointPath string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base URL must use http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("base URL must be absolute")
	}
	if parsed.User != nil {
		return "", errors.New("base URL must not contain user information")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("base URL must not contain a query or fragment")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/") + endpointPath
	parsed.RawPath = ""
	return parsed.String(), nil
}

func workerHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	configured := *client
	configured.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &configured
}

type ttsAudioMetadata struct {
	sampleRate  int
	channels    int
	sampleWidth int
	durationMS  int
	warnings    []string
}

func parseTTSAudioMetadata(header http.Header) (ttsAudioMetadata, error) {
	sampleRate, err := parsePositiveHeader(header, "X-Audio-Sample-Rate")
	if err != nil {
		return ttsAudioMetadata{}, err
	}
	channels, err := parsePositiveHeader(header, "X-Audio-Channels")
	if err != nil {
		return ttsAudioMetadata{}, err
	}
	bitsPerSample, err := parsePositiveHeader(header, "X-Audio-Bits-Per-Sample")
	if err != nil {
		return ttsAudioMetadata{}, err
	}
	if bitsPerSample%8 != 0 {
		return ttsAudioMetadata{}, fmt.Errorf(
			"invalid X-Audio-Bits-Per-Sample header %q",
			header.Get("X-Audio-Bits-Per-Sample"),
		)
	}
	durationMS, err := parseNonNegativeHeader(header, "X-Audio-Duration-Ms")
	if err != nil {
		return ttsAudioMetadata{}, err
	}
	return ttsAudioMetadata{
		sampleRate:  sampleRate,
		channels:    channels,
		sampleWidth: bitsPerSample / 8,
		durationMS:  durationMS,
		warnings:    splitHeaderList(header.Get("X-Audio-Warnings")),
	}, nil
}

func parsePositiveHeader(header http.Header, name string) (int, error) {
	value := strings.TrimSpace(header.Get(name))
	if value == "" {
		return 0, fmt.Errorf("missing %s header", name)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid %s header %q", name, value)
	}
	return parsed, nil
}

func parseNonNegativeHeader(header http.Header, name string) (int, error) {
	value := strings.TrimSpace(header.Get(name))
	if value == "" {
		return 0, fmt.Errorf("missing %s header", name)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid %s header %q", name, value)
	}
	return parsed, nil
}

func parseOptionalNonNegativeHeader(header http.Header, name string) (int, error) {
	value := strings.TrimSpace(header.Get(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid %s header %q", name, value)
	}
	return parsed, nil
}

func parseOptionalUint32Header(header http.Header, name string) (*uint32, error) {
	value := strings.TrimSpace(header.Get(name))
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid %s header %q", name, value)
	}
	result := uint32(parsed)
	return &result, nil
}

func parseOptionalBoolHeader(header http.Header, name string) (*bool, error) {
	value := strings.TrimSpace(header.Get(name))
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s header %q", name, value)
	}
	return &parsed, nil
}

func splitHeaderList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var values []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

func readBoundedResponse(response *http.Response, limit int64) ([]byte, error) {
	if response.ContentLength > limit {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrWorkerResponseTooLarge, limit)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrWorkerResponseTooLarge, limit)
	}
	return body, nil
}

func workerStatusError(worker string, response *http.Response) error {
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxWorkerErrorBodyBytes+1))
	truncated := int64(len(body)) > maxWorkerErrorBodyBytes
	if truncated {
		body = body[:maxWorkerErrorBodyBytes]
	}
	if readErr != nil {
		truncated = true
	}
	return &WorkerHTTPError{
		Worker:        worker,
		StatusCode:    response.StatusCode,
		Body:          strings.TrimSpace(string(body)),
		BodyTruncated: truncated,
	}
}

var (
	_ TTSClient = (*OmniVoiceHTTPClient)(nil)
	_ STTClient = (*STTHTTPClient)(nil)
)
