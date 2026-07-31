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
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const endpointTestFB2 = `<?xml version="1.0" encoding="UTF-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description>
    <title-info>
      <book-title>Тестовая книга</book-title>
      <author>
        <first-name>Иван</first-name>
        <last-name>Тестов</last-name>
      </author>
    </title-info>
  </description>
  <body>
    <section>
      <title><p>Первая глава</p></title>
      <p>Первая часть. * * * Вторая часть.</p>
    </section>
  </body>
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

	// Sixteen little-endian signed 16-bit mono samples: the minimum native
	// FLAC block size accepted consistently by libFLAC and FFmpeg.
	pcm := make([]byte, 16*2)
	for index := 0; index < 16; index++ {
		binary.LittleEndian.PutUint16(
			pcm[index*2:index*2+2],
			uint16(index),
		)
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

	result := make([]TTSRequest, len(f.requests))
	copy(result, f.requests)
	for index := range result {
		result[index].ReferenceAudio = slices.Clone(
			result[index].ReferenceAudio,
		)
	}

	return result
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
	mismatchFirst := f.mismatchFirst
	f.mu.Unlock()

	transcript := request.ExpectedText
	if mismatchFirst && request.ExpectedText == "Первая часть." {
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

	result := make([]STTRequest, len(f.requests))
	copy(result, f.requests)
	for index := range result {
		result[index].AudioPCM = slices.Clone(result[index].AudioPCM)
	}

	return result
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
		QueueSize:     4,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	return endpointTestFixture{server: server, tts: tts, stt: stt}
}

func TestEndpointsGenerateCompletesAsynchronously(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	fixture.stt.SetMismatchFirst(false)
	handler := fixture.server.Handler()

	bookResponse := endpointTestDo(
		t,
		handler,
		endpointTestMultipartRequest(
			t,
			http.MethodPost,
			"/v1/book",
			"file",
			"book.fb2",
			[]byte(endpointTestFB2),
			nil,
		),
	)
	if bookResponse.Code != http.StatusCreated {
		t.Fatalf(
			"POST book status = %d, body = %s",
			bookResponse.Code,
			bookResponse.Body,
		)
	}
	var book BookResource
	endpointTestDecodeJSON(t, bookResponse, &book)

	voiceResponse := endpointTestDo(
		t,
		handler,
		endpointTestMultipartRequest(
			t,
			http.MethodPost,
			"/v1/voice",
			"reference_audio",
			"voice.wav",
			endpointTestReferenceWAV(),
			map[string]string{"reference_text": "Эталонная фраза."},
		),
	)
	if voiceResponse.Code != http.StatusCreated {
		t.Fatalf(
			"POST voice status = %d, body = %s",
			voiceResponse.Code,
			voiceResponse.Body,
		)
	}
	var voice VoiceResource
	endpointTestDecodeJSON(t, voiceResponse, &voice)

	generationResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/generate/book/"+book.ID+"/voice/"+voice.ID,
		nil,
		"",
	)
	if generationResponse.Code != http.StatusAccepted {
		t.Fatalf(
			"POST generation status = %d, body = %s",
			generationResponse.Code,
			generationResponse.Body,
		)
	}
	var generation GenerationResponse
	endpointTestDecodeJSON(t, generationResponse, &generation)
	if generation.Job.Status != JobStatusQueued {
		t.Errorf(
			"accepted generation status = %q, want queued",
			generation.Job.Status,
		)
	}
	endpointTestAssertJobCounters(t, generation.Job)

	completed := endpointTestWaitForJob(
		t,
		handler,
		generation.JobID,
		JobStatusCompleted,
	)
	if completed.FragmentsCount != 2 ||
		completed.FragmentsPending != 0 ||
		completed.FragmentsReady != 2 ||
		completed.FragmentsWarnings != 0 ||
		completed.FragmentsFailed != 0 {
		t.Errorf("completed job counters = %+v", completed)
	}
	if len(fixture.tts.Requests()) != 2 || len(fixture.stt.Requests()) != 2 {
		t.Errorf(
			"worker calls = TTS:%d STT:%d, want 2 and 2",
			len(fixture.tts.Requests()),
			len(fixture.stt.Requests()),
		)
	}

	warningsResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/warnings",
		nil,
		"",
	)
	if warningsResponse.Code != http.StatusOK {
		t.Fatalf("GET warnings status = %d", warningsResponse.Code)
	}
	var warnings WarningsResponse
	endpointTestDecodeJSON(t, warningsResponse, &warnings)
	if warnings.Fragments == nil || len(warnings.Fragments) != 0 {
		t.Errorf("successful job warnings = %#v, want non-nil empty array", warnings)
	}
}

func TestEndpointsCompleteBookGenerationLifecycle(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()

	bookResponse := endpointTestDo(
		t,
		handler,
		endpointTestMultipartRequest(
			t,
			http.MethodPost,
			"/v1/book",
			"file",
			"book.fb2",
			[]byte(endpointTestFB2),
			nil,
		),
	)
	if bookResponse.Code != http.StatusCreated {
		t.Fatalf(
			"POST /v1/book status = %d, body = %s",
			bookResponse.Code,
			bookResponse.Body,
		)
	}
	var book BookResource
	endpointTestDecodeJSON(t, bookResponse, &book)
	if book.ID != "test-id-001" {
		t.Errorf("book id = %q, want test-id-001", book.ID)
	}
	if book.Title != "Тестовая книга" {
		t.Errorf("book title = %q", book.Title)
	}
	if !slices.Equal(book.Authors, []string{"Иван Тестов"}) {
		t.Errorf("book authors = %q", book.Authors)
	}
	if book.ChaptersCount != 1 || book.FragmentsCount != 2 {
		t.Errorf(
			"book counts = chapters:%d fragments:%d, want 1 and 2",
			book.ChaptersCount,
			book.FragmentsCount,
		)
	}
	if location := bookResponse.Header().Get("Location"); location !=
		"/v1/books/"+book.ID {
		t.Errorf("book Location = %q", location)
	}

	getBookResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/books/"+book.ID,
		nil,
		"",
	)
	if getBookResponse.Code != http.StatusOK {
		t.Fatalf("GET book status = %d", getBookResponse.Code)
	}
	var fetchedBook BookResource
	endpointTestDecodeJSON(t, getBookResponse, &fetchedBook)
	if fetchedBook.ID != book.ID ||
		fetchedBook.FragmentsCount != book.FragmentsCount {
		t.Errorf("GET book = %+v, want uploaded resource %+v", fetchedBook, book)
	}

	referenceAudio := endpointTestReferenceWAV()
	voiceResponse := endpointTestDo(
		t,
		handler,
		endpointTestMultipartRequest(
			t,
			http.MethodPost,
			"/v1/voice",
			"reference_audio",
			"voice.wav",
			referenceAudio,
			map[string]string{
				"name":           "Тестовый голос",
				"reference_text": "Эталонная фраза.",
			},
		),
	)
	if voiceResponse.Code != http.StatusCreated {
		t.Fatalf(
			"POST /v1/voice status = %d, body = %s",
			voiceResponse.Code,
			voiceResponse.Body,
		)
	}
	var voice VoiceResource
	endpointTestDecodeJSON(t, voiceResponse, &voice)
	if voice.ID != "test-id-002" ||
		voice.Name != "Тестовый голос" ||
		voice.Format != "wav" ||
		voice.ContentType != "audio/wav" ||
		!voice.HasReferenceText ||
		voice.SizeBytes != int64(len(referenceAudio)) {
		t.Errorf("voice resource = %+v", voice)
	}
	if strings.Contains(voiceResponse.Body.String(), "Эталонная фраза.") {
		t.Error("voice response leaked reference_text")
	}
	voiceLocation := voiceResponse.Header().Get("Location")
	if voiceLocation != "/v1/voices/"+voice.ID {
		t.Errorf("voice Location = %q", voiceLocation)
	}
	getVoiceResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		voiceLocation,
		nil,
		"",
	)
	if getVoiceResponse.Code != http.StatusOK {
		t.Fatalf(
			"GET voice status = %d, body = %s",
			getVoiceResponse.Code,
			getVoiceResponse.Body,
		)
	}
	var fetchedVoice VoiceResource
	endpointTestDecodeJSON(t, getVoiceResponse, &fetchedVoice)
	if fetchedVoice.ID != voice.ID ||
		fetchedVoice.Name != voice.Name ||
		fetchedVoice.SizeBytes != voice.SizeBytes {
		t.Errorf("GET voice = %+v, want uploaded resource %+v", fetchedVoice, voice)
	}

	voicesResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/voices",
		nil,
		"",
	)
	if voicesResponse.Code != http.StatusOK {
		t.Fatalf("GET voices status = %d", voicesResponse.Code)
	}
	var voices VoicesResponse
	endpointTestDecodeJSON(t, voicesResponse, &voices)
	if len(voices.Voices) != 1 || voices.Voices[0].ID != voice.ID {
		t.Errorf("GET voices = %+v", voices)
	}

	generationResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/generate/book/"+book.ID+"/voice/"+voice.ID,
		strings.NewReader(`{"automatic_warning_retries":0}`),
		"application/json",
	)
	if generationResponse.Code != http.StatusAccepted {
		t.Fatalf(
			"POST generation status = %d, body = %s",
			generationResponse.Code,
			generationResponse.Body,
		)
	}
	var generation GenerationResponse
	endpointTestDecodeJSON(t, generationResponse, &generation)
	if generation.JobID == "" || generation.Job.ID != generation.JobID {
		t.Fatalf("generation response has inconsistent job ids: %+v", generation)
	}
	if generation.Book.ID != book.ID || generation.Voice.ID != voice.ID {
		t.Errorf("generation resources = %+v", generation)
	}
	endpointTestAssertJobCounters(t, generation.Job)

	warningJob := endpointTestWaitForJob(
		t,
		handler,
		generation.JobID,
		JobStatusCompletedWithWarnings,
	)
	if warningJob.FragmentsCount != 2 ||
		warningJob.FragmentsPending != 0 ||
		warningJob.FragmentsReady != 1 ||
		warningJob.FragmentsWarnings != 1 ||
		warningJob.FragmentsFailed != 0 {
		t.Errorf("warning job counters = %+v", warningJob)
	}

	ttsRequests := fixture.tts.Requests()
	sttRequests := fixture.stt.Requests()
	if len(ttsRequests) != 2 || len(sttRequests) != 2 {
		t.Fatalf(
			"worker calls = TTS:%d STT:%d, want 2 and 2",
			len(ttsRequests),
			len(sttRequests),
		)
	}
	texts := []string{ttsRequests[0].Text, ttsRequests[1].Text}
	if !slices.Equal(texts, []string{"Первая часть.", "Вторая часть."}) {
		t.Errorf(
			"worker texts = %q; scene break must force a boundary and disappear",
			texts,
		)
	}
	for index, request := range ttsRequests {
		if strings.Contains(request.Text, "*") {
			t.Errorf("TTS request %d contains scene-break marker: %q", index, request.Text)
		}
		if request.JobID != generation.JobID ||
			request.FragmentID == "" ||
			request.ReferenceText != "Эталонная фраза." ||
			request.ReferenceContentType != "audio/wav" ||
			!bytes.Equal(request.ReferenceAudio, referenceAudio) {
			t.Errorf("TTS request %d = %+v", index, request)
		}
	}
	for index, request := range sttRequests {
		if request.JobID != generation.JobID ||
			request.FragmentID != ttsRequests[index].FragmentID ||
			request.ExpectedText != ttsRequests[index].Text ||
			request.SampleRate != 24_000 ||
			request.Channels != 1 ||
			request.SampleWidth != 2 {
			t.Errorf("STT request %d = %+v", index, request)
		}
	}

	archiveNotReady := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/audio.zip",
		nil,
		"",
	)
	endpointTestAssertProblem(
		t,
		archiveNotReady,
		http.StatusConflict,
		"JOB_NOT_READY",
	)
	chaptersNotReady := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/chapters",
		nil,
		"",
	)
	endpointTestAssertProblem(
		t,
		chaptersNotReady,
		http.StatusConflict,
		"JOB_NOT_READY",
	)

	warningsResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/warnings",
		nil,
		"",
	)
	if warningsResponse.Code != http.StatusOK {
		t.Fatalf("GET warnings status = %d", warningsResponse.Code)
	}
	var warnings WarningsResponse
	endpointTestDecodeJSON(t, warningsResponse, &warnings)
	if warnings.JobID != generation.JobID || len(warnings.Fragments) != 1 {
		t.Fatalf("warnings = %+v", warnings)
	}
	warning := warnings.Fragments[0]
	if warning.Text != "Первая часть." ||
		warning.STTText != "Текст распознан с ошибкой." ||
		warning.Status != FragmentStatusWarning ||
		warning.WarningCode != "transcript_mismatch" ||
		warning.Attempt != 1 {
		t.Errorf("warning fragment = %+v", warning)
	}

	editResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPatch,
		"/v1/fragment/"+warning.ID,
		strings.NewReader(`{"new_text":"  Исправленная часть.  "}`),
		"application/json",
	)
	if editResponse.Code != http.StatusOK {
		t.Fatalf(
			"PATCH fragment status = %d, body = %s",
			editResponse.Code,
			editResponse.Body,
		)
	}
	var edited FragmentResource
	endpointTestDecodeJSON(t, editResponse, &edited)
	if edited.Text != "Исправленная часть." ||
		edited.Status != FragmentStatusWarning ||
		edited.WarningCode != "text_edited" {
		t.Errorf("edited fragment = %+v", edited)
	}

	retryResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/job/"+generation.JobID+"/retry/warnings",
		strings.NewReader(
			fmt.Sprintf(`{"fragment_ids":[%q]}`, warning.ID),
		),
		"application/json",
	)
	if retryResponse.Code != http.StatusAccepted {
		t.Fatalf(
			"POST retry status = %d, body = %s",
			retryResponse.Code,
			retryResponse.Body,
		)
	}
	var retry RetryResponse
	endpointTestDecodeJSON(t, retryResponse, &retry)
	if retry.JobID != generation.JobID ||
		retry.Status != JobStatusQueued ||
		retry.FragmentsQueued != 1 {
		t.Errorf("retry response = %+v", retry)
	}

	completedJob := endpointTestWaitForJob(
		t,
		handler,
		generation.JobID,
		JobStatusCompleted,
	)
	if completedJob.FragmentsCount != 2 ||
		completedJob.FragmentsPending != 0 ||
		completedJob.FragmentsReady != 2 ||
		completedJob.FragmentsWarnings != 0 ||
		completedJob.FragmentsFailed != 0 {
		t.Errorf("completed job counters = %+v", completedJob)
	}
	if calls := fixture.tts.Requests(); len(calls) != 3 ||
		calls[2].Text != "Исправленная часть." ||
		calls[2].FragmentID != warning.ID {
		t.Errorf("TTS calls after retry = %+v", calls)
	}
	if calls := fixture.stt.Requests(); len(calls) != 3 ||
		calls[2].ExpectedText != "Исправленная часть." {
		t.Errorf("STT calls after retry = %+v", calls)
	}

	emptyWarningsResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/warnings",
		nil,
		"",
	)
	var emptyWarnings WarningsResponse
	endpointTestDecodeJSON(t, emptyWarningsResponse, &emptyWarnings)
	if emptyWarnings.Fragments == nil || len(emptyWarnings.Fragments) != 0 {
		t.Errorf("warnings after retry = %#v, want non-nil empty array", emptyWarnings)
	}

	zipResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/audio.zip",
		nil,
		"",
	)
	endpointTestAssertAudioZIP(
		t,
		zipResponse,
		book.ID,
		generation.JobID,
	)

	chaptersResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/chapters",
		nil,
		"",
	)
	if chaptersResponse.Code != http.StatusOK {
		t.Fatalf(
			"GET chapters status = %d, body = %s",
			chaptersResponse.Code,
			chaptersResponse.Body,
		)
	}
	var chapters JobChaptersResponse
	endpointTestDecodeJSON(t, chaptersResponse, &chapters)
	if chapters.JobID != generation.JobID ||
		chapters.BookID != book.ID ||
		chapters.AudioZIPURL != "/v1/job/"+generation.JobID+"/audio.zip" ||
		len(chapters.Chapters) != 1 {
		t.Fatalf("chapters response = %+v", chapters)
	}
	chapter := chapters.Chapters[0]
	if chapter.ChapterNumber != 1 ||
		chapter.Title != "Первая глава" ||
		chapter.FragmentsCount != 2 ||
		chapter.AudioFilename !=
			"character_0001_Первая глава.flac" ||
		chapter.AudioZIPURL !=
			"/v1/job/"+generation.JobID+"/chapters/1/audio.zip" {
		t.Errorf("chapter resource = %+v", chapter)
	}

	chapterZIPResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		chapter.AudioZIPURL,
		nil,
		"",
	)
	if chapterZIPResponse.Code != http.StatusOK {
		t.Fatalf(
			"GET chapter ZIP status = %d, body = %s",
			chapterZIPResponse.Code,
			chapterZIPResponse.Body,
		)
	}
	if disposition := chapterZIPResponse.Header().Get("Content-Disposition"); disposition != `attachment; filename="ready-chapter-flac-0001.zip"` {
		t.Errorf("chapter ZIP Content-Disposition = %q", disposition)
	}
	if bytes.Equal(chapterZIPResponse.Body.Bytes(), zipResponse.Body.Bytes()) {
		t.Error("chapter ZIP must have a scoped manifest distinct from full ZIP")
	}
	chapterZIPReader, err := zip.NewReader(
		bytes.NewReader(chapterZIPResponse.Body.Bytes()),
		int64(chapterZIPResponse.Body.Len()),
	)
	if err != nil {
		t.Fatalf("open chapter ZIP: %v", err)
	}
	var chapterManifest chapterArchiveManifest
	for _, file := range chapterZIPReader.File {
		if file.Name != "manifest.json" {
			continue
		}
		if err := json.Unmarshal(
			endpointTestReadZIPFile(t, file),
			&chapterManifest,
		); err != nil {
			t.Fatalf("decode chapter manifest: %v", err)
		}
	}
	if chapterManifest.Version != chapterArchiveManifestVersion ||
		chapterManifest.Scope != "chapter" ||
		chapterManifest.Chapter.ChapterNumber != 1 ||
		chapterManifest.Chapter.FragmentsCount != 2 {
		t.Errorf("chapter manifest = %+v", chapterManifest)
	}

	legacyZIPResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/book/"+book.ID,
		nil,
		"",
	)
	if legacyZIPResponse.Code != http.StatusOK ||
		!bytes.Equal(legacyZIPResponse.Body.Bytes(), zipResponse.Body.Bytes()) {
		t.Errorf(
			"legacy book ZIP status = %d, same archive = %v",
			legacyZIPResponse.Code,
			bytes.Equal(legacyZIPResponse.Body.Bytes(), zipResponse.Body.Bytes()),
		)
	}
}

func TestEndpointsHealthAndMethodRouting(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()

	healthRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRequest.Header.Set("X-Request-ID", "request-123")
	healthResponse := endpointTestDo(t, handler, healthRequest)
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d", healthResponse.Code)
	}
	if contentType := healthResponse.Header().Get("Content-Type"); !strings.HasPrefix(
		contentType,
		"application/json",
	) {
		t.Errorf("health Content-Type = %q", contentType)
	}
	if requestID := healthResponse.Header().Get("X-Request-ID"); requestID !=
		"request-123" {
		t.Errorf("health X-Request-ID = %q", requestID)
	}
	if got := healthResponse.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("health Cache-Control = %q", got)
	}
	if got := healthResponse.Header().Get("X-Content-Type-Options"); got !=
		"nosniff" {
		t.Errorf("health X-Content-Type-Options = %q", got)
	}
	var health map[string]string
	endpointTestDecodeJSON(t, healthResponse, &health)
	if health["status"] != "ok" {
		t.Errorf("health body = %#v", health)
	}

	invalidIDRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	invalidIDRequest.Header.Set("X-Request-ID", "invalid request id")
	invalidIDResponse := endpointTestDo(t, handler, invalidIDRequest)
	responseID := invalidIDResponse.Header().Get("X-Request-ID")
	if responseID == "invalid request id" ||
		!regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`).
			MatchString(responseID) {
		t.Errorf("sanitized X-Request-ID = %q", responseID)
	}

	tests := []struct {
		method    string
		path      string
		wantAllow string
	}{
		{http.MethodPost, "/healthz", http.MethodGet},
		{http.MethodGet, "/v1/book", http.MethodPost},
		{http.MethodPost, "/v1/voices", http.MethodGet},
		{http.MethodPut, "/v1/fragment/missing", http.MethodPatch},
		{http.MethodDelete, "/v1/job/missing", http.MethodGet},
	}
	for _, test := range tests {
		test := test
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := endpointTestRequest(
				t,
				handler,
				test.method,
				test.path,
				nil,
				"",
			)
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf(
					"status = %d, want %d; body = %s",
					response.Code,
					http.StatusMethodNotAllowed,
					response.Body,
				)
			}
			if allow := response.Header().Get("Allow"); !strings.Contains(
				allow,
				test.wantAllow,
			) {
				t.Errorf("Allow = %q, want it to contain %q", allow, test.wantAllow)
			}
		})
	}

	notFound := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/route-that-does-not-exist",
		nil,
		"",
	)
	if notFound.Code != http.StatusNotFound {
		t.Errorf("unknown route status = %d", notFound.Code)
	}
}

func TestRoutesFailsClosedWithoutPostgresConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	handler := Routes()
	response := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/healthz",
		nil,
		"",
	)
	endpointTestAssertProblem(
		t,
		response,
		http.StatusServiceUnavailable,
		"SERVICE_NOT_CONFIGURED",
	)
	if response.Header().Get("X-Request-ID") == "" {
		t.Error("configuration error has no request id")
	}
}

func TestEndpointsValidationAndStrictJSONErrors(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()

	t.Run("book requires multipart", func(t *testing.T) {
		response := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/book",
			strings.NewReader(endpointTestFB2),
			"application/xml",
		)
		endpointTestAssertProblem(
			t,
			response,
			http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE",
		)
	})

	t.Run("book requires file", func(t *testing.T) {
		response := endpointTestDo(
			t,
			handler,
			endpointTestMultipartRequest(
				t,
				http.MethodPost,
				"/v1/book",
				"",
				"",
				nil,
				map[string]string{"ignored": "value"},
			),
		)
		endpointTestAssertProblem(
			t,
			response,
			http.StatusBadRequest,
			"MISSING_FILE",
		)
	})

	t.Run("book must be valid FB2", func(t *testing.T) {
		response := endpointTestDo(
			t,
			handler,
			endpointTestMultipartRequest(
				t,
				http.MethodPost,
				"/v1/book",
				"file",
				"invalid.fb2",
				[]byte("<not-fb2/>"),
				nil,
			),
		)
		endpointTestAssertProblem(
			t,
			response,
			http.StatusUnprocessableEntity,
			"INVALID_FB2",
		)
	})

	bookResponse := endpointTestDo(
		t,
		handler,
		endpointTestMultipartRequest(
			t,
			http.MethodPost,
			"/v1/book",
			"file",
			"book.fb2",
			[]byte(endpointTestFB2),
			nil,
		),
	)
	var book BookResource
	endpointTestDecodeJSON(t, bookResponse, &book)

	t.Run("missing book", func(t *testing.T) {
		response := endpointTestRequest(
			t,
			handler,
			http.MethodGet,
			"/v1/books/missing",
			nil,
			"",
		)
		endpointTestAssertProblem(
			t,
			response,
			http.StatusNotFound,
			"BOOK_NOT_FOUND",
		)
	})

	t.Run("voice rejects unsupported audio", func(t *testing.T) {
		response := endpointTestDo(
			t,
			handler,
			endpointTestMultipartRequest(
				t,
				http.MethodPost,
				"/v1/voice",
				"reference_audio",
				"voice.mp3",
				[]byte("not audio"),
				map[string]string{"reference_text": "Фраза"},
			),
		)
		endpointTestAssertProblem(
			t,
			response,
			http.StatusUnsupportedMediaType,
			"UNSUPPORTED_VOICE_FORMAT",
		)
	})

	t.Run("missing voice", func(t *testing.T) {
		response := endpointTestRequest(
			t,
			handler,
			http.MethodGet,
			"/v1/voices/missing",
			nil,
			"",
		)
		endpointTestAssertProblem(
			t,
			response,
			http.StatusNotFound,
			"VOICE_NOT_FOUND",
		)
	})

	t.Run("voice requires reference text", func(t *testing.T) {
		response := endpointTestDo(
			t,
			handler,
			endpointTestMultipartRequest(
				t,
				http.MethodPost,
				"/v1/voice",
				"reference_audio",
				"voice.wav",
				endpointTestReferenceWAV(),
				nil,
			),
		)
		endpointTestAssertProblem(
			t,
			response,
			http.StatusUnprocessableEntity,
			"REFERENCE_TEXT_REQUIRED",
		)
	})

	t.Run("generate reports missing resources", func(t *testing.T) {
		missingBook := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/generate/book/missing/voice/missing",
			nil,
			"",
		)
		endpointTestAssertProblem(
			t,
			missingBook,
			http.StatusNotFound,
			"BOOK_NOT_FOUND",
		)

		missingVoice := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/generate/book/"+book.ID+"/voice/missing",
			nil,
			"",
		)
		endpointTestAssertProblem(
			t,
			missingVoice,
			http.StatusNotFound,
			"VOICE_NOT_FOUND",
		)
	})

	t.Run("missing jobs and archives", func(t *testing.T) {
		tests := []struct {
			path string
			code string
		}{
			{"/v1/job/missing", "JOB_NOT_FOUND"},
			{"/v1/job/missing/warnings", "JOB_NOT_FOUND"},
			{"/v1/job/missing/audio.zip", "JOB_NOT_FOUND"},
			{"/v1/book/missing", "BOOK_OR_JOB_NOT_FOUND"},
		}
		for _, test := range tests {
			response := endpointTestRequest(
				t,
				handler,
				http.MethodGet,
				test.path,
				nil,
				"",
			)
			endpointTestAssertProblem(
				t,
				response,
				http.StatusNotFound,
				test.code,
			)
		}
	})

	t.Run("fragment JSON is strict", func(t *testing.T) {
		tests := []struct {
			name string
			body string
		}{
			{"unknown field", `{"new_text":"текст","unknown":true}`},
			{"two objects", `{"new_text":"текст"}{"new_text":"ещё"}`},
			{"malformed", `{"new_text":`},
		}
		for _, test := range tests {
			test := test
			t.Run(test.name, func(t *testing.T) {
				response := endpointTestRequest(
					t,
					handler,
					http.MethodPatch,
					"/v1/fragment/missing",
					strings.NewReader(test.body),
					"application/json",
				)
				endpointTestAssertProblem(
					t,
					response,
					http.StatusBadRequest,
					"INVALID_JSON",
				)
			})
		}

		empty := endpointTestRequest(
			t,
			handler,
			http.MethodPatch,
			"/v1/fragment/missing",
			strings.NewReader(`{"new_text":"  "}`),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			empty,
			http.StatusUnprocessableEntity,
			"EMPTY_FRAGMENT_TEXT",
		)

		validMissing := endpointTestRequest(
			t,
			handler,
			http.MethodPatch,
			"/v1/fragment/missing",
			strings.NewReader(`{"new_text":"текст"}`),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			validMissing,
			http.StatusNotFound,
			"FRAGMENT_NOT_FOUND",
		)

		legacyMissing := endpointTestRequest(
			t,
			handler,
			"UPDATE",
			"/v1/fragmet/missing",
			strings.NewReader(`{"new_text":"текст"}`),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			legacyMissing,
			http.StatusNotFound,
			"FRAGMENT_NOT_FOUND",
		)
	})

	t.Run("retry JSON is strict", func(t *testing.T) {
		unknown := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/job/missing/retry/warnings",
			strings.NewReader(`{"unknown":true}`),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			unknown,
			http.StatusBadRequest,
			"INVALID_JSON",
		)

		missingJob := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/job/missing/retry/warnings",
			strings.NewReader(`{}`),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			missingJob,
			http.StatusNotFound,
			"JOB_OR_FRAGMENT_NOT_FOUND",
		)

		legacyMissingID := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/job/retry/warnings",
			strings.NewReader(`{"fragment_ids":[]}`),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			legacyMissingID,
			http.StatusUnprocessableEntity,
			"JOB_ID_REQUIRED",
		)
	})

	t.Run("JSON body size is bounded", func(t *testing.T) {
		body := strings.Repeat(" ", int(maxJSONBodySize)+1)
		response := endpointTestRequest(
			t,
			handler,
			http.MethodPatch,
			"/v1/fragment/missing",
			strings.NewReader(body),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			response,
			http.StatusRequestEntityTooLarge,
			"JSON_BODY_TOO_LARGE",
		)
	})
}

func endpointTestWaitForJob(
	t *testing.T,
	handler http.Handler,
	jobID string,
	want JobStatus,
) JobResource {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	var last JobResource
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
			t.Fatalf(
				"GET job status = %d, body = %s",
				response.Code,
				response.Body,
			)
		}
		endpointTestDecodeJSON(t, response, &last)
		endpointTestAssertJobCounters(t, last)
		if last.Status == want {
			return last
		}
		if last.Status == JobStatusCompleted ||
			last.Status == JobStatusCompletedWithWarnings ||
			last.Status == JobStatusFailed {
			t.Fatalf("job reached terminal status %q, want %q", last.Status, want)
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for job %q; last state = %+v", want, last)
		case <-ticker.C:
		}
	}
}

func endpointTestAssertJobCounters(t *testing.T, job JobResource) {
	t.Helper()

	sum := job.FragmentsPending +
		job.FragmentsReady +
		job.FragmentsWarnings +
		job.FragmentsFailed
	if sum != job.FragmentsCount {
		t.Fatalf(
			"job counter invariant failed: pending(%d) + ready(%d) + "+
				"warnings(%d) + failed(%d) = %d, fragments_count = %d",
			job.FragmentsPending,
			job.FragmentsReady,
			job.FragmentsWarnings,
			job.FragmentsFailed,
			sum,
			job.FragmentsCount,
		)
	}
}

func endpointTestAssertAudioZIP(
	t *testing.T,
	response *httptest.ResponseRecorder,
	bookID, jobID string,
) {
	t.Helper()

	if response.Code != http.StatusOK {
		t.Fatalf(
			"GET audio ZIP status = %d, body = %s",
			response.Code,
			response.Body,
		)
	}
	if contentType := response.Header().Get("Content-Type"); contentType !=
		"application/zip" {
		t.Errorf("ZIP Content-Type = %q", contentType)
	}
	if disposition := response.Header().Get("Content-Disposition"); disposition !=
		fmt.Sprintf(
			`attachment; filename="book-%s-chapters-flac.zip"`,
			bookID,
		) {
		t.Errorf("ZIP Content-Disposition = %q", disposition)
	}
	if response.Header().Get("Content-Length") !=
		fmt.Sprintf("%d", response.Body.Len()) {
		t.Errorf(
			"ZIP Content-Length = %q, body length = %d",
			response.Header().Get("Content-Length"),
			response.Body.Len(),
		)
	}

	reader, err := zip.NewReader(
		bytes.NewReader(response.Body.Bytes()),
		int64(response.Body.Len()),
	)
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}

	expectedNames := []string{
		"character_0001_Первая глава.flac",
		"manifest.json",
	}
	names := make([]string, 0, len(reader.File))
	var manifest archiveManifest
	for _, file := range reader.File {
		names = append(names, file.Name)
		if strings.HasPrefix(file.Name, "/") ||
			strings.Contains(file.Name, `\`) ||
			strings.Contains(file.Name, "..") {
			t.Errorf("unsafe ZIP path %q", file.Name)
		}

		data := endpointTestReadZIPFile(t, file)
		if strings.HasSuffix(file.Name, ".flac") {
			endpointTestAssertFLAC(t, file.Name, data)
			continue
		}
		if file.Name == "manifest.json" {
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatalf("decode manifest.json: %v", err)
			}
		}
	}
	if !slices.Equal(names, expectedNames) {
		t.Errorf("ZIP names = %q, want %q", names, expectedNames)
	}
	if manifest.Version != fullArchiveManifestVersion ||
		manifest.Book.ID != bookID ||
		manifest.Job.ID != jobID ||
		manifest.Job.Status != JobStatusCompleted ||
		len(manifest.Chapters) != 1 ||
		len(manifest.Fragments) != 2 {
		t.Errorf("archive manifest = %+v", manifest)
	}
	if len(manifest.Fragments) == 2 {
		if manifest.Fragments[0].Text != "Исправленная часть." ||
			manifest.Fragments[0].Attempt != 2 ||
			manifest.Fragments[0].Status != FragmentStatusReady {
			t.Errorf("first manifest fragment = %+v", manifest.Fragments[0])
		}
		if manifest.Fragments[1].Text != "Вторая часть." ||
			manifest.Fragments[1].Attempt != 1 ||
			manifest.Fragments[1].Status != FragmentStatusReady {
			t.Errorf("second manifest fragment = %+v", manifest.Fragments[1])
		}
	}
}

func endpointTestAssertFLAC(t *testing.T, name string, data []byte) {
	t.Helper()

	if len(data) < 4 || string(data[:4]) != "fLaC" {
		t.Errorf("%s has no native FLAC stream marker", name)
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
	fieldNames := make([]string, 0, len(fields))
	for name := range fields {
		fieldNames = append(fieldNames, name)
	}
	slices.Sort(fieldNames)
	for _, name := range fieldNames {
		if err := writer.WriteField(name, fields[name]); err != nil {
			t.Fatalf("write multipart field %q: %v", name, err)
		}
	}
	if fileField != "" {
		part, err := writer.CreateFormFile(fileField, filename)
		if err != nil {
			t.Fatalf("create multipart file %q: %v", fileField, err)
		}
		if _, err := part.Write(fileData); err != nil {
			t.Fatalf("write multipart file %q: %v", fileField, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(method, path, bytes.NewReader(body.Bytes()))
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

	decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
	if err := decoder.Decode(target); err != nil {
		t.Fatalf(
			"decode response JSON (status %d, body %q): %v",
			response.Code,
			response.Body.String(),
			err,
		)
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
		t.Fatalf(
			"problem status = %d, want %d; body = %s",
			response.Code,
			status,
			response.Body,
		)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(
		contentType,
		"application/json",
	) {
		t.Errorf("problem Content-Type = %q", contentType)
	}

	var problem ErrorResponse
	endpointTestDecodeJSON(t, response, &problem)
	if problem.Code != code {
		t.Errorf("problem code = %q, want %q; body = %+v", problem.Code, code, problem)
	}
	if problem.Error == "" || problem.RequestID == "" {
		t.Errorf("incomplete problem body = %+v", problem)
	}
	if response.Header().Get("X-Request-ID") != problem.RequestID {
		t.Errorf(
			"problem request id = %q, response header = %q",
			problem.RequestID,
			response.Header().Get("X-Request-ID"),
		)
	}
}
