package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultRewriteQueueSize    = 8
	maxRewritePromptLength     = 2_000
	maxRewriteSelection        = 1_000
	maxRewriteFailureLength    = 2_000
	defaultRewriterTimeout     = 30 * time.Minute
	rewriteModelsLookupTimeout = 10 * time.Second
)

type RewriteTaskStatus string

const (
	RewriteTaskStatusQueued              RewriteTaskStatus = "queued"
	RewriteTaskStatusRunning             RewriteTaskStatus = "running"
	RewriteTaskStatusCompleted           RewriteTaskStatus = "completed"
	RewriteTaskStatusCompletedWithErrors RewriteTaskStatus = "completed_with_errors"
	RewriteTaskStatusFailed              RewriteTaskStatus = "failed"
)

type RewriteFragmentStatus string

const (
	RewriteFragmentStatusQueued    RewriteFragmentStatus = "queued"
	RewriteFragmentStatusRunning   RewriteFragmentStatus = "running"
	RewriteFragmentStatusCompleted RewriteFragmentStatus = "completed"
	RewriteFragmentStatusFailed    RewriteFragmentStatus = "failed"
)

type RewriteTaskResource struct {
	ID                 string            `json:"id"`
	JobID              string            `json:"job_id"`
	ModelID            string            `json:"model_id"`
	ModelRevision      string            `json:"model_revision,omitempty"`
	Prompt             string            `json:"prompt"`
	Temperature        float64           `json:"temperature"`
	TopK               int               `json:"top_k"`
	TopP               float64           `json:"top_p"`
	MinP               float64           `json:"min_p"`
	RepeatPenalty      float64           `json:"repeat_penalty"`
	MaxTokens          int               `json:"max_tokens"`
	Status             RewriteTaskStatus `json:"status"`
	FragmentsCount     int               `json:"fragments_count"`
	FragmentsPending   int               `json:"fragments_pending"`
	FragmentsCompleted int               `json:"fragments_completed"`
	FragmentsFailed    int               `json:"fragments_failed"`
	Error              string            `json:"error,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

type RewriteTaskFragmentResource struct {
	FragmentID    string                `json:"fragment_id"`
	Ordinal       int                   `json:"ordinal"`
	Status        RewriteFragmentStatus `json:"status"`
	RevisionID    string                `json:"revision_id,omitempty"`
	Changed       bool                  `json:"changed"`
	Reason        string                `json:"reason,omitempty"`
	Error         string                `json:"error,omitempty"`
	DurationMS    int64                 `json:"duration_ms"`
	ModelRevision string                `json:"model_revision,omitempty"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

type RewriteTaskResponse struct {
	RewriteTaskResource
	Fragments []RewriteTaskFragmentResource `json:"fragments"`
}

type RewriteWarningsRequest struct {
	FragmentIDs   []string `json:"fragment_ids,omitempty"`
	ModelID       string   `json:"model_id,omitempty"`
	Prompt        string   `json:"prompt,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	MinP          *float64 `json:"min_p,omitempty"`
	RepeatPenalty *float64 `json:"repeat_penalty,omitempty"`
	MaxTokens     *int     `json:"max_tokens,omitempty"`
}

type rewriteTask struct {
	ID            string
	JobID         string
	FragmentIDs   []string
	ModelID       string
	Prompt        string
	Temperature   float64
	TopK          int
	TopP          float64
	MinP          float64
	RepeatPenalty float64
	MaxTokens     int
}

type rewriteWorkItem struct {
	RewriteID     string
	JobID         string
	Fragment      FragmentResource
	ModelID       string
	Prompt        string
	Temperature   float64
	TopK          int
	TopP          float64
	MinP          float64
	RepeatPenalty float64
	MaxTokens     int
}

type rewriteRunner struct {
	server *Server
	queue  chan rewriteTask

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	once   sync.Once
	wg     sync.WaitGroup
}

func newRewriteRunner(server *Server, queueSize int) *rewriteRunner {
	if queueSize <= 0 {
		queueSize = defaultRewriteQueueSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &rewriteRunner{
		server: server,
		queue:  make(chan rewriteTask, queueSize),
		ctx:    ctx,
		cancel: cancel,
	}
	runner.wg.Add(1)
	go runner.loop()
	return runner
}

func (r *rewriteRunner) enqueue(task rewriteTask) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	select {
	case r.queue <- task:
		return true
	default:
		return false
	}
}

func (r *rewriteRunner) close() {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.cancel()
		r.mu.Unlock()
		r.wg.Wait()
	})
}

func (r *rewriteRunner) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case task := <-r.queue:
			r.process(task)
		}
	}
}

func (r *rewriteRunner) process(task rewriteTask) {
	now := r.server.now().UTC()
	if err := r.server.store.startRewriteTask(r.ctx, task.ID, now); err != nil {
		r.server.logger.Error(
			"start rewrite task",
			"rewrite_id", task.ID,
			"job_id", task.JobID,
			"error", err,
		)
		return
	}

	for _, fragmentID := range task.FragmentIDs {
		if r.ctx.Err() != nil {
			return
		}
		if err := r.processFragment(task, fragmentID); err != nil {
			r.server.logger.Error(
				"rewrite fragment",
				"rewrite_id", task.ID,
				"job_id", task.JobID,
				"fragment_id", fragmentID,
				"error", err,
			)
		}
	}
	if err := r.server.store.finishRewriteTask(
		r.ctx,
		task.ID,
		r.server.now().UTC(),
	); err != nil {
		r.server.logger.Error(
			"finish rewrite task",
			"rewrite_id", task.ID,
			"job_id", task.JobID,
			"error", err,
		)
	}
}

func (r *rewriteRunner) processFragment(
	task rewriteTask,
	fragmentID string,
) error {
	item, err := r.server.store.startRewriteFragment(
		r.ctx,
		task.ID,
		fragmentID,
		r.server.now().UTC(),
	)
	if err != nil {
		r.failFragment(task.ID, fragmentID, "fragment is no longer rewriteable", err)
		return err
	}

	workerContext, cancel := context.WithTimeout(
		r.ctx,
		r.server.rewriterTimeout,
	)
	result, err := r.server.rewriter.Rewrite(
		workerContext,
		RewriterRequest{
			APIVersion:    "v1",
			RequestID:     r.server.newID(),
			FragmentID:    item.Fragment.ID,
			Text:          item.Fragment.Text,
			STTText:       item.Fragment.STTText,
			WarningCode:   item.Fragment.WarningCode,
			ModelID:       item.ModelID,
			Prompt:        item.Prompt,
			Temperature:   &item.Temperature,
			TopK:          &item.TopK,
			TopP:          &item.TopP,
			MinP:          &item.MinP,
			RepeatPenalty: &item.RepeatPenalty,
			MaxTokens:     &item.MaxTokens,
		},
	)
	cancel()
	if err != nil {
		r.failFragment(task.ID, fragmentID, "local text model failed", err)
		return fmt.Errorf("call rewriter worker: %w", err)
	}

	result.RewrittenText = strings.TrimSpace(result.RewrittenText)
	result.Reason = strings.TrimSpace(result.Reason)
	if result.RewrittenText == "" ||
		len([]rune(result.RewrittenText)) > maxFragmentLength ||
		result.Reason == "" ||
		len([]rune(result.Reason)) > maxRevisionReasonLength {
		err = errors.New("rewriter returned text or reason outside configured limits")
		r.failFragment(task.ID, fragmentID, "local text model returned invalid output", err)
		return err
	}
	if err := validateRewritePreservesPhrase(
		item.Fragment.Text,
		result.RewrittenText,
	); err != nil {
		r.failFragment(
			task.ID,
			fragmentID,
			"local text model did not preserve the complete phrase",
			err,
		)
		return err
	}

	revisionID := ""
	if result.RewrittenText != item.Fragment.Text {
		revisionID = r.server.newID()
	}
	if err := r.server.store.completeRewriteFragment(
		r.ctx,
		task.ID,
		fragmentID,
		item.Fragment.Text,
		revisionID,
		result,
		r.server.now().UTC(),
	); err != nil {
		r.failFragment(task.ID, fragmentID, "fragment changed while rewriting", err)
		return fmt.Errorf("save rewrite result: %w", err)
	}
	return nil
}

func (r *rewriteRunner) failFragment(
	rewriteID, fragmentID, publicMessage string,
	cause error,
) {
	r.server.logger.Error(
		"rewriter task failed",
		"rewrite_id", rewriteID,
		"fragment_id", fragmentID,
		"error", cause,
	)
	if err := r.server.store.failRewriteFragment(
		context.Background(),
		rewriteID,
		fragmentID,
		boundedMessage(publicMessage, maxRewriteFailureLength),
		r.server.now().UTC(),
	); err != nil {
		r.server.logger.Error(
			"persist rewrite failure",
			"rewrite_id", rewriteID,
			"fragment_id", fragmentID,
			"error", err,
		)
	}
}

func (s *Server) rewriteModels(
	writer http.ResponseWriter,
	request *http.Request,
) {
	ctx, cancel := context.WithTimeout(
		request.Context(),
		rewriteModelsLookupTimeout,
	)
	defer cancel()
	models, err := s.rewriter.Models(ctx)
	if err != nil {
		s.logger.Error(
			"list rewrite models",
			"request_id", requestID(request.Context()),
			"error", err,
		)
		writeProblem(
			writer,
			request,
			http.StatusServiceUnavailable,
			"REWRITER_UNAVAILABLE",
			"local text rewriter is unavailable",
		)
		return
	}
	writeJSON(writer, http.StatusOK, models)
}

func (s *Server) rewriteWarnings(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var input RewriteWarningsRequest
	if !decodeJSON(writer, request, &input, true) {
		return
	}
	if len(input.FragmentIDs) > maxRewriteSelection {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"TOO_MANY_FRAGMENTS",
			"too many fragment_ids were selected",
		)
		return
	}
	for index := range input.FragmentIDs {
		input.FragmentIDs[index] = strings.TrimSpace(input.FragmentIDs[index])
		if input.FragmentIDs[index] == "" {
			writeProblem(
				writer,
				request,
				http.StatusUnprocessableEntity,
				"INVALID_FRAGMENT_ID",
				"fragment_ids must not contain empty values",
			)
			return
		}
	}

	ctx, cancel := context.WithTimeout(
		request.Context(),
		rewriteModelsLookupTimeout,
	)
	models, err := s.rewriter.Models(ctx)
	cancel()
	if err != nil {
		s.logger.Error(
			"validate rewrite model",
			"request_id", requestID(request.Context()),
			"error", err,
		)
		writeProblem(
			writer,
			request,
			http.StatusServiceUnavailable,
			"REWRITER_UNAVAILABLE",
			"local text rewriter is unavailable",
		)
		return
	}
	input.ModelID = strings.TrimSpace(input.ModelID)
	if input.ModelID == "" {
		input.ModelID = models.DefaultModelID
	}
	if !rewriteModelExists(models.Models, input.ModelID) {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"MODEL_NOT_ALLOWED",
			"model_id is not in the server allowlist",
		)
		return
	}
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" {
		input.Prompt = models.DefaultPrompt
	}
	if len([]rune(input.Prompt)) > maxRewritePromptLength {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"REWRITE_PROMPT_TOO_LONG",
			"prompt is too long",
		)
		return
	}
	temperature := models.Settings.Temperature.Default
	if input.Temperature != nil {
		temperature = *input.Temperature
	}
	if math.IsNaN(temperature) ||
		math.IsInf(temperature, 0) ||
		temperature < models.Settings.Temperature.Minimum ||
		temperature > models.Settings.Temperature.Maximum {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_REWRITE_TEMPERATURE",
			"temperature is outside the worker-supported range",
		)
		return
	}
	topK := models.Settings.TopK.Default
	if input.TopK != nil {
		topK = *input.TopK
	}
	if topK < models.Settings.TopK.Minimum ||
		topK > models.Settings.TopK.Maximum {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_REWRITE_TOP_K",
			"top_k is outside the worker-supported range",
		)
		return
	}
	topP, ok := resolveRewriteFloatSetting(
		writer,
		request,
		"top_p",
		"INVALID_REWRITE_TOP_P",
		input.TopP,
		models.Settings.TopP,
	)
	if !ok {
		return
	}
	minP, ok := resolveRewriteFloatSetting(
		writer,
		request,
		"min_p",
		"INVALID_REWRITE_MIN_P",
		input.MinP,
		models.Settings.MinP,
	)
	if !ok {
		return
	}
	repeatPenalty, ok := resolveRewriteFloatSetting(
		writer,
		request,
		"repeat_penalty",
		"INVALID_REWRITE_REPEAT_PENALTY",
		input.RepeatPenalty,
		models.Settings.RepeatPenalty,
	)
	if !ok {
		return
	}
	maxTokens := models.Settings.MaxTokens.Default
	if input.MaxTokens != nil {
		maxTokens = *input.MaxTokens
	}
	if maxTokens < models.Settings.MaxTokens.Minimum ||
		maxTokens > models.Settings.MaxTokens.Maximum {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_REWRITE_MAX_TOKENS",
			"max_tokens is outside the worker-supported range",
		)
		return
	}

	rewriteID := s.newID()
	resource, task, err := s.store.createRewriteTask(
		request.Context(),
		rewriteID,
		request.PathValue("jobID"),
		input.FragmentIDs,
		input.ModelID,
		input.Prompt,
		temperature,
		topK,
		topP,
		minP,
		repeatPenalty,
		maxTokens,
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
			"INVALID_REWRITE_SELECTION",
			err.Error(),
		)
		return
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"REWRITE_NOT_ALLOWED",
			err.Error(),
		)
		return
	case err != nil:
		s.internalStoreError(writer, request, "create rewrite task", err)
		return
	}

	if !s.rewriteRunner.enqueue(task) {
		if deleteErr := s.store.deleteRewriteTask(
			request.Context(),
			rewriteID,
		); deleteErr != nil {
			s.logger.Error(
				"rollback unscheduled rewrite",
				"request_id", requestID(request.Context()),
				"rewrite_id", rewriteID,
				"error", deleteErr,
			)
		}
		writeProblem(
			writer,
			request,
			http.StatusServiceUnavailable,
			"REWRITE_QUEUE_FULL",
			"rewrite queue is full",
		)
		return
	}

	writer.Header().Set("Location", "/v1/rewrite/"+rewriteID)
	writeJSON(writer, http.StatusAccepted, RewriteTaskResponse{
		RewriteTaskResource: resource,
		Fragments:           initialRewriteFragments(task.FragmentIDs),
	})
}

func resolveRewriteFloatSetting(
	writer http.ResponseWriter,
	request *http.Request,
	name string,
	code string,
	requested *float64,
	supported FloatSettingRange,
) (float64, bool) {
	value := supported.Default
	if requested != nil {
		value = *requested
	}
	if math.IsNaN(value) ||
		math.IsInf(value, 0) ||
		value < supported.Minimum ||
		value > supported.Maximum {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			code,
			name+" is outside the worker-supported range",
		)
		return 0, false
	}
	return value, true
}

func (s *Server) rewriteStatus(
	writer http.ResponseWriter,
	request *http.Request,
) {
	task, ok, err := s.store.rewriteTask(
		request.Context(),
		request.PathValue("rewriteID"),
	)
	if err != nil {
		s.internalStoreError(writer, request, "get rewrite task", err)
		return
	}
	if !ok {
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"REWRITE_NOT_FOUND",
			"rewrite task not found",
		)
		return
	}
	writeJSON(writer, http.StatusOK, task)
}

func rewriteModelExists(models []RewriteModelResource, id string) bool {
	return slices.ContainsFunc(models, func(model RewriteModelResource) bool {
		return model.ID == id
	})
}

func initialRewriteFragments(ids []string) []RewriteTaskFragmentResource {
	result := make([]RewriteTaskFragmentResource, 0, len(ids))
	for index, id := range ids {
		result = append(result, RewriteTaskFragmentResource{
			FragmentID: id,
			Ordinal:    index + 1,
			Status:     RewriteFragmentStatusQueued,
		})
	}
	return result
}

func boundedMessage(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}
