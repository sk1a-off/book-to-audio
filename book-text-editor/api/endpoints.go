package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"book-text-editor/internal/fb2"
	"book-text-editor/internal/segment"
)

const (
	maxUploadSize     int64 = 50 << 20
	maxVoiceSize      int64 = 5 << 20
	maxJSONBodySize   int64 = 1 << 20
	maxFragmentLength       = 20_000
	defaultQueueSize        = 16
)

type ErrorResponse struct {
	Code      string `json:"code"`
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// Dependencies contains replaceable worker and platform boundaries.
type Dependencies struct {
	TTS              TTSClient
	STT              STTClient
	Rewriter         RewriterClient
	Logger           *slog.Logger
	ID               func() string
	Now              func() time.Time
	WorkerTimeout    time.Duration
	RewriterTimeout  time.Duration
	QueueSize        int
	RewriteQueueSize int
}

// Server owns the HTTP API and bounded orchestration queues.
type Server struct {
	store           repository
	parser          fb2.Parser
	tts             TTSClient
	stt             STTClient
	rewriter        RewriterClient
	logger          *slog.Logger
	newID           func() string
	now             func() time.Time
	workerTimeout   time.Duration
	rewriterTimeout time.Duration

	handler       http.Handler
	runner        *jobRunner
	fragments     *fragmentService
	rewriteRunner *rewriteRunner
}

// NewServer constructs an isolated API instance backed by memory storage.
func NewServer(dependencies Dependencies) (*Server, error) {
	return newServerWithRepository(dependencies, newMemoryStore())
}

func newServerWithRepository(
	dependencies Dependencies,
	store repository,
) (*Server, error) {
	if store == nil {
		return nil, errors.New("repository is required")
	}
	if dependencies.TTS == nil {
		return nil, errors.New("TTS client is required")
	}
	if dependencies.STT == nil {
		return nil, errors.New("STT client is required")
	}

	rewriter := dependencies.Rewriter
	if rewriter == nil {
		rewriter = unavailableRewriterClient{}
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	newID := dependencies.ID
	if newID == nil {
		newID = randomID
	}
	now := dependencies.Now
	if now == nil {
		now = time.Now
	}
	workerTimeout := dependencies.WorkerTimeout
	if workerTimeout <= 0 {
		workerTimeout = 10 * time.Minute
	}
	rewriterTimeout := dependencies.RewriterTimeout
	if rewriterTimeout <= 0 {
		rewriterTimeout = defaultRewriterTimeout
	}
	queueSize := dependencies.QueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	rewriteQueueSize := dependencies.RewriteQueueSize
	if rewriteQueueSize <= 0 {
		rewriteQueueSize = defaultRewriteQueueSize
	}

	server := &Server{
		store:           store,
		parser:          fb2.NewParser(segment.Segmenter{}),
		tts:             dependencies.TTS,
		stt:             dependencies.STT,
		rewriter:        rewriter,
		logger:          logger,
		newID:           newID,
		now:             now,
		workerTimeout:   workerTimeout,
		rewriterTimeout: rewriterTimeout,
	}
	server.runner = newJobRunner(server, queueSize)
	fragments, err := newFragmentService(store, server.runner.enqueue)
	if err != nil {
		server.runner.close()
		return nil, fmt.Errorf("configure fragment workflow: %w", err)
	}
	server.fragments = fragments
	server.rewriteRunner = newRewriteRunner(server, rewriteQueueSize)
	server.handler = server.routes()

	return server, nil
}

// NewServerFromEnv creates a PostgreSQL-backed server and HTTP worker clients.
func NewServerFromEnv(logger *slog.Logger) (*Server, error) {
	workerHTTPClient := &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	tts, err := NewOmniVoiceHTTPClient(
		envOrDefault("OMNIVOICE_URL", "http://127.0.0.1:8001"),
		workerHTTPClient,
	)
	if err != nil {
		return nil, fmt.Errorf("configure OmniVoice client: %w", err)
	}
	stt, err := NewSTTHTTPClient(
		envOrDefault("STT_URL", "http://127.0.0.1:8002"),
		workerHTTPClient,
	)
	if err != nil {
		return nil, fmt.Errorf("configure STT client: %w", err)
	}
	rewriter, err := NewRewriterHTTPClient(
		envOrDefault("REWRITER_URL", "http://127.0.0.1:8003"),
		&http.Client{
			Timeout: defaultRewriterTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("configure rewriter client: %w", err)
	}

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := OpenPostgresStore(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL repository: %w", err)
	}

	server, err := newServerWithRepository(Dependencies{
		TTS:      tts,
		STT:      stt,
		Rewriter: rewriter,
		Logger:   logger,
	}, store)
	if err != nil {
		store.close()
		return nil, err
	}
	return server, nil
}

// Routes preserves the package-level entry point used by the application.
func Routes() http.Handler {
	server, err := NewServerFromEnv(slog.Default())
	if err == nil {
		return server.Handler()
	}
	return requestMiddleware(slog.Default(), http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			writeProblem(
				writer,
				request,
				http.StatusServiceUnavailable,
				"SERVICE_NOT_CONFIGURED",
				"API dependencies are not configured",
			)
		},
	))
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Close() {
	s.runner.close()
	s.rewriteRunner.close()
	s.store.close()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	registerUIRoutes(mux)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/book", s.uploadBook)
	mux.HandleFunc("GET /v1/books/{bookID}", s.getBook)
	mux.HandleFunc("POST /v1/voice", s.uploadVoice)
	mux.HandleFunc("GET /v1/voices", s.getVoices)
	mux.HandleFunc("GET /v1/voices/{voiceID}", s.getVoice)
	mux.HandleFunc("DELETE /v1/voices/{voiceID}", s.deleteVoiceEndpoint)
	mux.HandleFunc("GET /v1/jobs", s.listJobs)
	mux.HandleFunc(
		"POST /v1/generate/book/{bookID}/voice/{voiceID}",
		s.generate,
	)
	mux.HandleFunc("GET /v1/job/{jobID}", s.jobStatus)
	mux.HandleFunc("DELETE /v1/job/{jobID}", s.deleteJobEndpoint)
	mux.HandleFunc("GET /v1/job/{jobID}/warnings", s.jobWarnings)
	mux.HandleFunc("GET /v1/job/{jobID}/chapters", s.listJobChapters)
	mux.HandleFunc(
		"GET /v1/job/{jobID}/chapters/{chapterNumber}",
		s.getJobChapter,
	)
	mux.HandleFunc(
		"GET /v1/job/{jobID}/chapters/{chapterNumber}/audio.flac",
		s.getChapterAudioFLAC,
	)
	// Compatibility path. It now returns FLAC bytes, never a ZIP archive.
	mux.HandleFunc(
		"GET /v1/job/{jobID}/chapters/{chapterNumber}/audio.zip",
		s.getChapterAudioFLAC,
	)
	mux.HandleFunc("PATCH /v1/fragment/{fragmentID}", s.editFragment)
	mux.HandleFunc(
		"GET /v1/fragment/{fragmentID}/audio.wav",
		s.getFragmentAudio,
	)
	mux.HandleFunc(
		"POST /v1/fragment/{fragmentID}/approve",
		s.approveFragment,
	)
	mux.HandleFunc(
		"GET /v1/fragment/{fragmentID}/reviews",
		s.listFragmentManualReviews,
	)
	mux.HandleFunc(
		"GET /v1/fragment/{fragmentID}/revisions",
		s.listFragmentRevisions,
	)
	mux.HandleFunc(
		"POST /v1/fragment/{fragmentID}/revisions/{revisionID}/restore",
		s.restoreFragmentRevision,
	)
	mux.HandleFunc(
		"POST /v1/job/{jobID}/retry/warnings",
		s.retryWarnings,
	)
	mux.HandleFunc("GET /v1/rewrite/models", s.rewriteModels)
	mux.HandleFunc(
		"POST /v1/job/{jobID}/rewrite/warnings",
		s.rewriteWarnings,
	)
	mux.HandleFunc("GET /v1/rewrite/{rewriteID}", s.rewriteStatus)
	mux.HandleFunc("GET /v1/job/{jobID}/audio.zip", s.getAudioZIP)

	// Compatibility routes for the first public draft.
	mux.HandleFunc("UPDATE /v1/fragmet/{fragmentID}", s.editFragment)
	mux.HandleFunc("POST /v1/job/retry/warnings", s.retryWarningsLegacy)
	mux.HandleFunc("GET /v1/book/{bookID}", s.getLatestBookAudioZIP)

	return requestMiddleware(s.logger, mux)
}

func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) uploadBook(writer http.ResponseWriter, request *http.Request) {
	file, header, cleanup, ok := multipartFile(
		writer,
		request,
		maxUploadSize,
		"file",
	)
	if !ok {
		return
	}
	defer cleanup()

	parsed, err := parseBookUpload(file, header.Filename, s.parser, maxUploadSize)
	if err != nil {
		status, code, message := bookUploadProblem(err)
		writeProblem(writer, request, status, code, message)
		return
	}
	id := s.newID()
	resource, err := s.store.createBook(
		request.Context(),
		id,
		parsed,
		s.now().UTC(),
	)
	if err != nil {
		s.internalStoreError(writer, request, "create book", err)
		return
	}
	writer.Header().Set("Location", "/v1/books/"+id)
	writeJSON(writer, http.StatusCreated, resource)
}

func (s *Server) getBook(writer http.ResponseWriter, request *http.Request) {
	resource, ok, err := s.store.book(
		request.Context(),
		request.PathValue("bookID"),
	)
	if err != nil {
		s.internalStoreError(writer, request, "get book", err)
		return
	}
	if !ok {
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"BOOK_NOT_FOUND",
			"book not found",
		)
		return
	}
	writeJSON(writer, http.StatusOK, resource)
}

func (s *Server) uploadVoice(writer http.ResponseWriter, request *http.Request) {
	file, header, cleanup, ok := multipartFile(
		writer,
		request,
		maxVoiceSize,
		"reference_audio",
		"file",
	)
	if !ok {
		return
	}
	defer cleanup()

	audio, err := readBounded(file, maxVoiceSize)
	if err != nil {
		writeProblem(
			writer,
			request,
			http.StatusRequestEntityTooLarge,
			"VOICE_TOO_LARGE",
			"voice reference exceeds the upload limit",
		)
		return
	}
	format, contentType, ok := detectVoiceFormat(audio)
	if !ok {
		writeProblem(
			writer,
			request,
			http.StatusUnsupportedMediaType,
			"UNSUPPORTED_VOICE_FORMAT",
			"voice reference must be a FLAC or PCM WAV file",
		)
		return
	}

	name := strings.TrimSpace(request.FormValue("name"))
	if name == "" {
		name = strings.TrimSpace(header.Filename)
	}
	if name == "" {
		name = "Voice reference"
	}
	if len([]rune(name)) > 200 {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_VOICE_NAME",
			"voice name is too long",
		)
		return
	}

	referenceText := strings.Join(
		strings.Fields(request.FormValue("reference_text")),
		" ",
	)
	if referenceText == "" {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"REFERENCE_TEXT_REQUIRED",
			"reference_text is required for voice cloning",
		)
		return
	}
	if len([]rune(referenceText)) > 4_000 {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"REFERENCE_TEXT_TOO_LONG",
			"reference_text is too long",
		)
		return
	}

	id := s.newID()
	resource, err := s.store.createVoice(
		request.Context(),
		id,
		name,
		format,
		contentType,
		referenceText,
		audio,
		s.now().UTC(),
	)
	if err != nil {
		s.internalStoreError(writer, request, "create voice", err)
		return
	}
	writer.Header().Set("Location", "/v1/voices/"+id)
	writeJSON(writer, http.StatusCreated, resource)
}

func (s *Server) getVoices(writer http.ResponseWriter, request *http.Request) {
	voices, err := s.store.voices(request.Context())
	if err != nil {
		s.internalStoreError(writer, request, "list voices", err)
		return
	}
	writeJSON(writer, http.StatusOK, VoicesResponse{Voices: voices})
}

func (s *Server) getVoice(writer http.ResponseWriter, request *http.Request) {
	voice, ok, err := s.store.voice(
		request.Context(),
		request.PathValue("voiceID"),
	)
	if err != nil {
		s.internalStoreError(writer, request, "get voice", err)
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
	writeJSON(writer, http.StatusOK, voice)
}

func (s *Server) generate(writer http.ResponseWriter, request *http.Request) {
	settings, valid := decodeGenerationSettings(writer, request)
	if !valid {
		return
	}
	bookID := request.PathValue("bookID")
	voiceID := request.PathValue("voiceID")

	bookResource, ok, err := s.store.book(request.Context(), bookID)
	if err != nil {
		s.internalStoreError(writer, request, "get book", err)
		return
	}
	if !ok {
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"BOOK_NOT_FOUND",
			"book not found",
		)
		return
	}
	voiceResource, ok, err := s.store.voice(request.Context(), voiceID)
	if err != nil {
		s.internalStoreError(writer, request, "get voice", err)
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

	jobID := s.newID()
	fragmentIDs := make([]string, bookResource.FragmentsCount)
	originalRevisionIDs := make([]string, bookResource.FragmentsCount)
	for index := range fragmentIDs {
		fragmentIDs[index] = s.newID()
		originalRevisionIDs[index] = s.newID()
	}
	job, task, err := s.store.createJob(
		request.Context(),
		jobID,
		bookID,
		voiceID,
		fragmentIDs,
		originalRevisionIDs,
		settings,
		s.now().UTC(),
	)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeProblem(
				writer,
				request,
				http.StatusNotFound,
				"BOOK_OR_VOICE_NOT_FOUND",
				err.Error(),
			)
		case errors.Is(err, errInvalid), errors.Is(err, errConflict):
			writeProblem(
				writer,
				request,
				http.StatusConflict,
				"JOB_NOT_CREATED",
				err.Error(),
			)
		default:
			s.internalStoreError(writer, request, "create job", err)
		}
		return
	}
	if !s.runner.enqueue(task) {
		if rollbackErr := s.store.deleteJob(request.Context(), jobID); rollbackErr != nil {
			s.logger.Error(
				"rollback unscheduled job",
				"request_id", requestID(request.Context()),
				"job_id", jobID,
				"error", rollbackErr,
			)
		}
		writeProblem(
			writer,
			request,
			http.StatusServiceUnavailable,
			"JOB_QUEUE_FULL",
			"generation queue is full",
		)
		return
	}

	writer.Header().Set("Location", "/v1/job/"+jobID)
	writeJSON(writer, http.StatusAccepted, GenerationResponse{
		Book:  bookResource,
		Voice: voiceResource,
		Job:   job,
		JobID: jobID,
	})
}

func (s *Server) jobStatus(writer http.ResponseWriter, request *http.Request) {
	job, ok, err := s.store.job(
		request.Context(),
		request.PathValue("jobID"),
	)
	if err != nil {
		s.internalStoreError(writer, request, "get job", err)
		return
	}
	if !ok {
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"JOB_NOT_FOUND",
			"job not found",
		)
		return
	}
	writeJSON(writer, http.StatusOK, job)
}

func (s *Server) jobWarnings(writer http.ResponseWriter, request *http.Request) {
	jobID := request.PathValue("jobID")
	fragments, ok, err := s.store.jobIssues(request.Context(), jobID)
	if err != nil {
		s.internalStoreError(writer, request, "list job warnings", err)
		return
	}
	if !ok {
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"JOB_NOT_FOUND",
			"job not found",
		)
		return
	}
	writeJSON(writer, http.StatusOK, WarningsResponse{
		JobID:     jobID,
		Fragments: fragments,
	})
}

func (s *Server) editFragment(writer http.ResponseWriter, request *http.Request) {
	var input EditFragmentRequest
	if !decodeJSON(writer, request, &input, false) {
		return
	}
	input.NewText = strings.Join(strings.Fields(input.NewText), " ")
	if input.NewText == "" {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"EMPTY_FRAGMENT_TEXT",
			"new_text must not be empty",
		)
		return
	}
	if len([]rune(input.NewText)) > maxFragmentLength {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"FRAGMENT_TEXT_TOO_LONG",
			"new_text is too long",
		)
		return
	}

	now := s.now().UTC()
	fragment, err := s.fragments.editAndQueue(
		request.Context(),
		request.PathValue("fragmentID"),
		input.NewText,
		s.newID(),
		now,
	)
	switch {
	case errors.Is(err, errNotFound):
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"FRAGMENT_NOT_FOUND",
			"fragment not found",
		)
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"FRAGMENT_BUSY",
			err.Error(),
		)
	case errors.Is(err, errGenerationQueueFull):
		s.logger.Warn(
			"edited fragment was saved but generation queue is full",
			"request_id", requestID(request.Context()),
			"job_id", fragment.JobID,
			"fragment_id", fragment.ID,
			"error", err,
		)
		writeProblem(
			writer,
			request,
			http.StatusServiceUnavailable,
			"FRAGMENT_SAVED_QUEUE_FULL",
			"fragment text was saved, but the generation queue is full; retry the fragment later",
		)
	case err != nil:
		s.internalStoreError(writer, request, "edit and queue fragment", err)
	default:
		writer.Header().Set("X-Fragment-Regeneration-Queued", "true")
		writeJSON(writer, http.StatusOK, fragment)
	}
}

func (s *Server) retryWarnings(writer http.ResponseWriter, request *http.Request) {
	s.retryWarningsForJob(writer, request, request.PathValue("jobID"))
}

func (s *Server) retryWarningsLegacy(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var input LegacyRetryRequest
	if !decodeJSON(writer, request, &input, false) {
		return
	}
	if input.JobID == "" {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"JOB_ID_REQUIRED",
			"job_id is required",
		)
		return
	}
	s.scheduleRetry(writer, request, input.JobID, input.FragmentIDs)
}

func (s *Server) retryWarningsForJob(
	writer http.ResponseWriter,
	request *http.Request,
	jobID string,
) {
	var input RetryWarningsRequest
	if !decodeJSON(writer, request, &input, true) {
		return
	}
	s.scheduleRetry(writer, request, jobID, input.FragmentIDs)
}

func (s *Server) scheduleRetry(
	writer http.ResponseWriter,
	request *http.Request,
	jobID string,
	fragmentIDs []string,
) {
	task, err := s.store.prepareRetry(
		request.Context(),
		jobID,
		fragmentIDs,
		s.now().UTC(),
	)
	switch {
	case errors.Is(err, errNotFound):
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"JOB_OR_FRAGMENT_NOT_FOUND",
			err.Error(),
		)
		return
	case errors.Is(err, errInvalid):
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_RETRY_SELECTION",
			err.Error(),
		)
		return
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"RETRY_NOT_ALLOWED",
			err.Error(),
		)
		return
	case err != nil:
		s.internalStoreError(writer, request, "prepare retry", err)
		return
	}

	if !s.runner.enqueue(task) {
		if cancelErr := s.store.cancelRetry(
			request.Context(),
			task,
			s.now().UTC(),
		); cancelErr != nil {
			s.logger.Error(
				"rollback unscheduled retry",
				"request_id", requestID(request.Context()),
				"job_id", jobID,
				"error", cancelErr,
			)
		}
		writeProblem(
			writer,
			request,
			http.StatusServiceUnavailable,
			"JOB_QUEUE_FULL",
			"generation queue is full",
		)
		return
	}
	writeJSON(writer, http.StatusAccepted, RetryResponse{
		JobID:           jobID,
		Status:          JobStatusQueued,
		FragmentsQueued: len(task.FragmentIDs),
	})
}

func (s *Server) getAudioZIP(writer http.ResponseWriter, request *http.Request) {
	s.writeJobArchive(writer, request, request.PathValue("jobID"))
}

func (s *Server) getLatestBookAudioZIP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	jobID, ok, err := s.store.latestJobID(
		request.Context(),
		request.PathValue("bookID"),
	)
	if err != nil {
		s.internalStoreError(writer, request, "get latest book job", err)
		return
	}
	if !ok {
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"BOOK_OR_JOB_NOT_FOUND",
			"book has no generation job",
		)
		return
	}
	s.writeJobArchive(writer, request, jobID)
}

func (s *Server) writeJobArchive(
	writer http.ResponseWriter,
	request *http.Request,
	jobID string,
) {
	snapshot, err := s.store.archiveSnapshot(request.Context(), jobID)
	switch {
	case errors.Is(err, errNotFound):
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"JOB_NOT_FOUND",
			"job not found",
		)
		return
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"JOB_NOT_READY",
			err.Error(),
		)
		return
	case err != nil:
		s.internalStoreError(writer, request, "prepare audio archive", err)
		return
	}

	archive, err := buildAudioZIP(snapshot)
	if err != nil {
		s.logger.Error(
			"build audio archive",
			"request_id", requestID(request.Context()),
			"job_id", jobID,
			"error", err,
		)
		writeProblem(
			writer,
			request,
			http.StatusInternalServerError,
			"ARCHIVE_ERROR",
			"could not prepare the audio archive",
		)
		return
	}
	writer.Header().Set("Content-Type", "application/zip")
	writer.Header().Set(
		"Content-Disposition",
		fmt.Sprintf(
			`attachment; filename="book-%s-chapters-flac.zip"`,
			snapshot.Book.ID,
		),
	)
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(archive)
}

func multipartFile(
	writer http.ResponseWriter,
	request *http.Request,
	limit int64,
	fields ...string,
) (multipart.File, *multipart.FileHeader, func(), bool) {
	contentType := request.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		writeProblem(
			writer,
			request,
			http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE",
			"request must use multipart/form-data",
		)
		return nil, nil, func() {}, false
	}

	request.Body = http.MaxBytesReader(writer, request.Body, limit+(1<<20))
	if err := request.ParseMultipartForm(limit); err != nil {
		status := http.StatusBadRequest
		code := "INVALID_MULTIPART"
		message := "could not parse multipart request"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			status = http.StatusRequestEntityTooLarge
			code = "UPLOAD_TOO_LARGE"
			message = "request exceeds the upload limit"
		}
		writeProblem(writer, request, status, code, message)
		return nil, nil, func() {}, false
	}
	cleanup := func() {
		if request.MultipartForm != nil {
			_ = request.MultipartForm.RemoveAll()
		}
	}
	for _, field := range fields {
		file, header, err := request.FormFile(field)
		if err == nil {
			return file, header, func() {
				_ = file.Close()
				cleanup()
			}, true
		}
		if !errors.Is(err, http.ErrMissingFile) {
			cleanup()
			writeProblem(
				writer,
				request,
				http.StatusBadRequest,
				"INVALID_FILE",
				fmt.Sprintf("could not read multipart field %q", field),
			)
			return nil, nil, func() {}, false
		}
	}
	cleanup()
	if len(fields) == 0 {
		writeProblem(
			writer,
			request,
			http.StatusInternalServerError,
			"INTERNAL_ERROR",
			"file upload is not configured",
		)
		return nil, nil, func() {}, false
	}
	writeProblem(
		writer,
		request,
		http.StatusBadRequest,
		"MISSING_FILE",
		fmt.Sprintf(
			"one of multipart fields %q is required",
			strings.Join(fields, `", "`),
		),
	)
	return nil, nil, func() {}, false
}

func readBounded(reader multipart.File, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

func detectVoiceFormat(data []byte) (format, contentType string, ok bool) {
	switch {
	case len(data) >= 4 && string(data[:4]) == "fLaC":
		return "flac", "audio/flac", true
	case len(data) >= 12 &&
		string(data[:4]) == "RIFF" &&
		string(data[8:12]) == "WAVE":
		return "wav", "audio/wav", true
	default:
		return "", "", false
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (s *Server) workerContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.runner.context(), s.workerTimeout)
}

func (s *Server) internalStoreError(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
	err error,
) {
	s.logger.Error(
		operation,
		"request_id", requestID(request.Context()),
		"error", err,
	)
	writeProblem(
		writer,
		request,
		http.StatusInternalServerError,
		"STORAGE_ERROR",
		"could not access persistent state",
	)
}
