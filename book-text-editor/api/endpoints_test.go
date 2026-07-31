package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const endpointTestFB2 = `<?xml version="1.0" encoding="UTF-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description><title-info><book-title>Тестовая книга</book-title>
  <author><first-name>Иван</first-name><last-name>Тестов</last-name></author>
  </title-info></description>
  <body><section><title><p>Первая глава</p></title>
  <p>Первая часть. * * * Вторая часть.</p></section></body>
</FictionBook>`

type endpointTestIDGenerator struct {
	mu   sync.Mutex
	next int
}

func (g *endpointTestIDGenerator) New() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return fmt.Sprintf("test-id-%03d", g.next)
}

type endpointTestTTS struct {
	mu       sync.Mutex
	requests []TTSRequest
}

func (f *endpointTestTTS) Generate(
	ctx context.Context,
	request TTSRequest,
) (TTSResult, error) {
	if err := ctx.Err(); err != nil {
		return TTSResult{}, err
	}
	request.ReferenceAudio = slices.Clone(request.ReferenceAudio)
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	pcm := make([]byte, 16*2)
	for index := 0; index < 16; index++ {
		binary.LittleEndian.PutUint16(pcm[index*2:index*2+2], uint16(index))
	}
	return TTSResult{
		RequestID:   request.RequestID,
		AudioPCM:    pcm,
		SampleRate:  24_000,
		Channels:    1,
		SampleWidth: 2,
		DurationMS:  1,
	}, nil
}

func (f *endpointTestTTS) Requests() []TTSRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

type endpointTestSTT struct {
	mu            sync.Mutex
	requests      []STTRequest
	mismatchFirst bool
}

func (f *endpointTestSTT) Transcribe(
	ctx context.Context,
	request STTRequest,
) (STTResult, error) {
	if err := ctx.Err(); err != nil {
		return STTResult{}, err
	}
	request.AudioPCM = slices.Clone(request.AudioPCM)
	f.mu.Lock()
	f.requests = append(f.requests, request)
	mismatch := f.mismatchFirst && request.ExpectedText == "Первая часть."
	f.mu.Unlock()
	transcript := request.ExpectedText
	if mismatch {
		transcript = "Текст распознан с ошибкой."
	}
	return STTResult{
		RequestID:  request.RequestID,
		Text:       transcript,
		Language:   "ru",
		DurationMS: 1,
	}, nil
}

func (f *endpointTestSTT) Requests() []STTRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *endpointTestSTT) SetMismatchFirst(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mismatchFirst = value
}

type endpointTestFixture struct {
	server *Server
	tts    *endpointTestTTS
	stt    *endpointTestSTT
}

func newEndpointTestFixture(t *testing.T) endpointTestFixture {
	t.Helper()
	tts := &endpointTestTTS{}
	stt := &endpointTestSTT{mismatchFirst: true}
	ids := &endpointTestIDGenerator{}
	server, err := NewServer(Dependencies{
		TTS: tts,
		STT: stt,
		Logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
		ID: ids.New,
		Now: func() time.Time {
			return time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
		},
		WorkerTimeout: time.Second,
		QueueSize:     8,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)
	return endpointTestFixture{server: server, tts: tts, stt: stt}
}

func TestEndpointsGenerateAndDownloadDirectChapterFLAC(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	fixture.stt.SetMismatchFirst(false)
	handler := fixture.server.Handler()
	book, voice := endpointTestUploadInputs(t, handler)

	response := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/generate/book/"+book.ID+"/voice/"+voice.ID,
		strings.NewReader(`{"automatic_warning_retries":0}`),
		"application/json",
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("generate status=%d body=%s", response.Code, response.Body)
	}
	var generation GenerationResponse
	endpointTestDecodeJSON(t, response, &generation)
	job := endpointTestWaitForJob(t, handler, generation.JobID, JobStatusCompleted)
	endpointTestAssertJobCounters(t, job)

	catalogResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/chapters",
		nil,
		"",
	)
	if catalogResponse.Code != http.StatusOK {
		t.Fatalf("catalog status=%d body=%s", catalogResponse.Code, catalogResponse.Body)
	}
	var catalog JobChaptersResponse
	endpointTestDecodeJSON(t, catalogResponse, &catalog)
	if len(catalog.Chapters) != 1 || !catalog.Chapters[0].Ready ||
		len(catalog.Fragments) != 2 {
		t.Fatalf("catalog=%+v", catalog)
	}
	if !strings.HasSuffix(catalog.Chapters[0].AudioURL, "/audio.flac") {
		t.Fatalf("chapter audio URL=%q", catalog.Chapters[0].AudioURL)
	}

	flacResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		catalog.Chapters[0].AudioURL,
		nil,
		"",
	)
	if flacResponse.Code != http.StatusOK {
		t.Fatalf("chapter FLAC status=%d body=%s", flacResponse.Code, flacResponse.Body)
	}
	if got := flacResponse.Header().Get("Content-Type"); got != "audio/flac" {
		t.Errorf("Content-Type=%q", got)
	}
	endpointTestAssertFLAC(t, "chapter", flacResponse.Body.Bytes())
}

func TestReadyFragmentCanBeEditedAndAutomaticallyRequeued(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	fixture.stt.SetMismatchFirst(false)
	handler := fixture.server.Handler()
	book, voice := endpointTestUploadInputs(t, handler)
	generationResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/generate/book/"+book.ID+"/voice/"+voice.ID,
		nil,
		"",
	)
	var generation GenerationResponse
	endpointTestDecodeJSON(t, generationResponse, &generation)
	endpointTestWaitForJob(t, handler, generation.JobID, JobStatusCompleted)

	catalogResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/chapters",
		nil,
		"",
	)
	var catalog JobChaptersResponse
	endpointTestDecodeJSON(t, catalogResponse, &catalog)
	fragmentID := catalog.Fragments[0].ID

	editResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPatch,
		"/v1/fragment/"+fragmentID,
		strings.NewReader(`{"new_text":"Исправленная первая часть."}`),
		"application/json",
	)
	if editResponse.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", editResponse.Code, editResponse.Body)
	}
	if editResponse.Header().Get("X-Fragment-Regeneration-Queued") != "true" {
		t.Error("edited fragment was not queued")
	}
	endpointTestWaitForJob(t, handler, generation.JobID, JobStatusCompleted)
	if len(fixture.tts.Requests()) < 3 {
		t.Fatalf("TTS calls=%d, want regeneration", len(fixture.tts.Requests()))
	}
}

func endpointTestUploadInputs(
	t *testing.T,
	handler http.Handler,
) (BookResource, VoiceResource) {
	t.Helper()
	bookResponse := endpointTestDo(t, handler, endpointTestMultipartRequest(
		t,
		http.MethodPost,
		"/v1/book",
		"file",
		"book.fb2",
		[]byte(endpointTestFB2),
		nil,
	))
	if bookResponse.Code != http.StatusCreated {
		t.Fatalf("upload book status=%d body=%s", bookResponse.Code, bookResponse.Body)
	}
	var book BookResource
	endpointTestDecodeJSON(t, bookResponse, &book)
	voiceResponse := endpointTestDo(t, handler, endpointTestMultipartRequest(
		t,
		http.MethodPost,
		"/v1/voice",
		"reference_audio",
		"voice.wav",
		endpointTestReferenceWAV(),
		map[string]string{"name": "Тестовый голос", "reference_text": "Эталонная фраза."},
	))
	if voiceResponse.Code != http.StatusCreated {
		t.Fatalf("upload voice status=%d body=%s", voiceResponse.Code, voiceResponse.Body)
	}
	var voice VoiceResource
	endpointTestDecodeJSON(t, voiceResponse, &voice)
	return book, voice
}

func endpointTestWaitForJob(
	t *testing.T,
	handler http.Handler,
	jobID string,
	want JobStatus,
) JobResource {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		response := endpointTestRequest(
			t,
			handler,
			http.MethodGet,
			"/v1/job/"+jobID,
			nil,
			"",
		)
		if response.Code != http.StatusOK {
			t.Fatalf("GET job status=%d body=%s", response.Code, response.Body)
		}
		var job JobResource
		endpointTestDecodeJSON(t, response, &job)
		if job.Status == want {
			return job
		}
		if TERMINAL := job.Status == JobStatusFailed || job.Status == JobStatusCompletedWithWarnings; TERMINAL && job.Status != want {
			t.Fatalf("job reached %q, want %q", job.Status, want)
		}
		select {
		case <-deadline.C:
			t.Fatalf("job did not reach %q; last=%+v", want, job)
		case <-ticker.C:
		}
	}
}

func endpointTestAssertJobCounters(t *testing.T, job JobResource) {
	t.Helper()
	sum := job.FragmentsPending + job.FragmentsReady +
		job.FragmentsWarnings + job.FragmentsFailed
	if sum != job.FragmentsCount {
		t.Errorf("job counters sum=%d, count=%d", sum, job.FragmentsCount)
	}
}

func endpointTestAssertAudioZIP(
	t *testing.T,
	response *httptest.ResponseRecorder,
	bookID, jobID string,
) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("ZIP status=%d body=%s", response.Code, response.Body)
	}
	reader, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil {
		t.Fatalf("open ZIP: %v", err)
	}
	if len(reader.File) == 0 {
		t.Fatal("ZIP is empty")
	}
	_ = bookID
	_ = jobID
}

func endpointTestAssertFLAC(t *testing.T, name string, data []byte) {
	t.Helper()
	if len(data) < 4 || string(data[:4]) != "fLaC" {
		t.Errorf("%s has no native FLAC marker", name)
	}
}

func endpointTestReadZIPFile(t *testing.T, file *zip.File) []byte {
	t.Helper()
	reader, err := file.Open()
	if err != nil {
		t.Fatalf("open ZIP entry %q: %v", file.Name, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read ZIP entry %q: %v", file.Name, err)
	}
	return data
}

func endpointTestReferenceWAV() []byte {
	pcm := []byte{0x00, 0x00, 0x01, 0x00}
	result := make([]byte, 44+len(pcm))
	copy(result[0:4], "RIFF")
	binary.LittleEndian.PutUint32(result[4:8], uint32(36+len(pcm)))
	copy(result[8:12], "WAVE")
	copy(result[12:16], "fmt ")
	binary.LittleEndian.PutUint32(result[16:20], 16)
	binary.LittleEndian.PutUint16(result[20:22], 1)
	binary.LittleEndian.PutUint16(result[22:24], 1)
	binary.LittleEndian.PutUint32(result[24:28], 24_000)
	binary.LittleEndian.PutUint32(result[28:32], 48_000)
	binary.LittleEndian.PutUint16(result[32:34], 2)
	binary.LittleEndian.PutUint16(result[34:36], 16)
	copy(result[36:40], "data")
	binary.LittleEndian.PutUint32(result[40:44], uint32(len(pcm)))
	copy(result[44:], pcm)
	return result
}

func endpointTestMultipartRequest(
	t *testing.T,
	method, path, fileField, filename string,
	fileData []byte,
	fields map[string]string,
) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(fileField, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(fileData); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func endpointTestRequest(
	t *testing.T,
	handler http.Handler,
	method, path string,
	body io.Reader,
	contentType string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, body)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return endpointTestDo(t, handler, request)
}

func endpointTestDo(
	t *testing.T,
	handler http.Handler,
	request *http.Request,
) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func endpointTestDecodeJSON(
	t *testing.T,
	response *httptest.ResponseRecorder,
	target any,
) {
	t.Helper()
	decoder := json.NewDecoder(response.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, response.Body)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("response contains trailing JSON: %v", err)
	}
}

func endpointTestAssertProblem(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	code string,
) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("problem status=%d, want=%d body=%s", response.Code, status, response.Body)
	}
	var problem ErrorResponse
	endpointTestDecodeJSON(t, response, &problem)
	if problem.Code != code {
		t.Fatalf("problem code=%q, want=%q", problem.Code, code)
	}
}
