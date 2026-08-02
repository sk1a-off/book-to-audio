package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const (
	defaultJobsPageSize = 50
	maxJobsPageSize     = 100
)

// JobsResponse is a bounded page of jobs together with the number of jobs
// matching the requested status filter.
type JobsResponse struct {
	Jobs   []JobResource `json:"jobs"`
	Total  int64         `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

// QueueJobResource adds presentation metadata to a durable generation job.
type QueueJobResource struct {
	Job           JobResource `json:"job"`
	BookTitle     string      `json:"book_title"`
	QueuePosition int         `json:"queue_position"`
}

// QueueFragmentResource is the lightweight queue view. It intentionally does
// not contain PCM bytes, voice prompts, or other large worker payloads.
type QueueFragmentResource struct {
	Fragment      FragmentResource `json:"fragment"`
	BookID        string           `json:"book_id"`
	BookTitle     string           `json:"book_title"`
	ChapterTitle  string           `json:"chapter_title"`
	JobStatus     JobStatus        `json:"job_status"`
	QueuePosition int              `json:"queue_position"`
}

// GenerationQueueResponse is a point-in-time view of the shared FIFO queue.
type GenerationQueueResponse struct {
	Jobs           []QueueJobResource      `json:"jobs"`
	Fragments      []QueueFragmentResource `json:"fragments"`
	TotalJobs      int                     `json:"total_jobs"`
	TotalFragments int                     `json:"total_fragments"`
}

type jobListFilter struct {
	Statuses []JobStatus
	Limit    int
	Offset   int
}

type jobListPage struct {
	Jobs  []JobResource
	Total int64
}

// listJobs handles GET /v1/jobs.
func (s *Server) listJobs(writer http.ResponseWriter, request *http.Request) {
	filter, err := parseJobListFilter(request.URL.Query())
	if err != nil {
		writeProblem(
			writer,
			request,
			http.StatusBadRequest,
			"INVALID_QUERY",
			err.Error(),
		)
		return
	}

	page, err := s.store.listJobs(request.Context(), filter)
	if err != nil {
		s.internalStoreError(writer, request, "list jobs", err)
		return
	}
	if page.Jobs == nil {
		page.Jobs = make([]JobResource, 0)
	}

	writeJSON(writer, http.StatusOK, JobsResponse{
		Jobs:   page.Jobs,
		Total:  page.Total,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	})
}

// generationQueue handles GET /v1/queue.
func (s *Server) generationQueue(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		writeProblem(
			writer,
			request,
			http.StatusBadRequest,
			"INVALID_QUERY",
			"generation queue does not accept query parameters",
		)
		return
	}
	snapshot, err := s.store.generationQueue(request.Context())
	if err != nil {
		s.internalStoreError(writer, request, "get generation queue", err)
		return
	}
	if snapshot.Jobs == nil {
		snapshot.Jobs = make([]QueueJobResource, 0)
	}
	if snapshot.Fragments == nil {
		snapshot.Fragments = make([]QueueFragmentResource, 0)
	}
	writeJSON(writer, http.StatusOK, GenerationQueueResponse{
		Jobs:           snapshot.Jobs,
		Fragments:      snapshot.Fragments,
		TotalJobs:      len(snapshot.Jobs),
		TotalFragments: len(snapshot.Fragments),
	})
}

func (s *Server) pauseJobEndpoint(writer http.ResponseWriter, request *http.Request) {
	jobID := request.PathValue("jobID")
	resource, err := s.store.pauseJob(
		request.Context(),
		jobID,
		s.now().UTC(),
	)
	if !s.handleJobTransitionError(writer, request, err, "pause job") {
		return
	}
	s.runner.interrupt(jobID)
	writeJSON(writer, http.StatusOK, resource)
}

func (s *Server) resumeJobEndpoint(writer http.ResponseWriter, request *http.Request) {
	jobID := request.PathValue("jobID")
	resource, err := s.store.resumeJob(
		request.Context(),
		jobID,
		s.now().UTC(),
	)
	if !s.handleJobTransitionError(writer, request, err, "resume job") {
		return
	}
	s.runner.enqueue(jobTask{JobID: jobID})
	writeJSON(writer, http.StatusOK, resource)
}

func (s *Server) cancelJobEndpoint(writer http.ResponseWriter, request *http.Request) {
	jobID := request.PathValue("jobID")
	resource, err := s.store.cancelJob(
		request.Context(),
		jobID,
		s.now().UTC(),
	)
	if !s.handleJobTransitionError(writer, request, err, "cancel job") {
		return
	}
	s.runner.interrupt(jobID)
	writeJSON(writer, http.StatusOK, resource)
}

func (s *Server) handleJobTransitionError(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
	operation string,
) bool {
	switch {
	case errors.Is(err, errNotFound):
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"JOB_NOT_FOUND",
			"job not found",
		)
		return false
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"JOB_TRANSITION_NOT_ALLOWED",
			err.Error(),
		)
		return false
	case err != nil:
		s.internalStoreError(writer, request, operation, err)
		return false
	default:
		return true
	}
}

func parseJobListFilter(query url.Values) (jobListFilter, error) {
	for key := range query {
		switch key {
		case "status", "limit", "offset":
		default:
			return jobListFilter{}, fmt.Errorf(
				"unsupported query parameter %q",
				key,
			)
		}
	}

	statuses, err := parseJobListStatuses(query)
	if err != nil {
		return jobListFilter{}, err
	}
	limit, err := parseJobListInteger(
		query,
		"limit",
		defaultJobsPageSize,
		1,
		maxJobsPageSize,
	)
	if err != nil {
		return jobListFilter{}, err
	}
	offset, err := parseJobListInteger(query, "offset", 0, 0, -1)
	if err != nil {
		return jobListFilter{}, err
	}

	filter := jobListFilter{
		Statuses: statuses,
		Limit:    limit,
		Offset:   offset,
	}
	if err := filter.validate(); err != nil {
		return jobListFilter{}, err
	}
	return filter, nil
}

func parseJobListStatuses(query url.Values) ([]JobStatus, error) {
	values, exists := query["status"]
	if !exists {
		return nil, nil
	}
	if len(values) != 1 || values[0] == "" {
		return nil, errors.New("status must be specified exactly once")
	}

	switch values[0] {
	case "all":
		return nil, nil
	case "active":
		return []JobStatus{
			JobStatusQueued,
			JobStatusRunning,
			JobStatusPaused,
		}, nil
	case string(JobStatusQueued):
		return []JobStatus{JobStatusQueued}, nil
	case string(JobStatusRunning):
		return []JobStatus{JobStatusRunning}, nil
	case string(JobStatusPaused):
		return []JobStatus{JobStatusPaused}, nil
	case string(JobStatusCanceled):
		return []JobStatus{JobStatusCanceled}, nil
	case string(JobStatusCompleted):
		return []JobStatus{JobStatusCompleted}, nil
	case string(JobStatusCompletedWithWarnings):
		return []JobStatus{JobStatusCompletedWithWarnings}, nil
	case string(JobStatusFailed):
		return []JobStatus{JobStatusFailed}, nil
	default:
		return nil, fmt.Errorf("unsupported status %q", values[0])
	}
}

func parseJobListInteger(
	query url.Values,
	name string,
	defaultValue, minimum, maximum int,
) (int, error) {
	values, exists := query[name]
	if !exists {
		return defaultValue, nil
	}
	if len(values) != 1 || values[0] == "" {
		return 0, fmt.Errorf("%s must be specified exactly once", name)
	}
	for _, character := range values[0] {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("%s must be a decimal integer", name)
		}
	}

	value, err := strconv.Atoi(values[0])
	if err != nil || value < minimum || maximum >= 0 && value > maximum {
		if maximum >= 0 {
			return 0, fmt.Errorf(
				"%s must be between %d and %d",
				name,
				minimum,
				maximum,
			)
		}
		return 0, fmt.Errorf("%s must be at least %d", name, minimum)
	}
	return value, nil
}

func (filter jobListFilter) validate() error {
	if filter.Limit < 1 || filter.Limit > maxJobsPageSize {
		return fmt.Errorf(
			"%w: jobs limit must be between 1 and %d",
			errInvalid,
			maxJobsPageSize,
		)
	}
	if filter.Offset < 0 {
		return fmt.Errorf("%w: jobs offset must not be negative", errInvalid)
	}

	seen := make(map[JobStatus]struct{}, len(filter.Statuses))
	for _, status := range filter.Statuses {
		switch status {
		case JobStatusQueued,
			JobStatusRunning,
			JobStatusPaused,
			JobStatusCanceled,
			JobStatusCompleted,
			JobStatusCompletedWithWarnings,
			JobStatusFailed:
		default:
			return fmt.Errorf(
				"%w: unsupported job status %q",
				errInvalid,
				status,
			)
		}
		if _, duplicate := seen[status]; duplicate {
			return fmt.Errorf(
				"%w: duplicate job status %q",
				errInvalid,
				status,
			)
		}
		seen[status] = struct{}{}
	}
	return nil
}
