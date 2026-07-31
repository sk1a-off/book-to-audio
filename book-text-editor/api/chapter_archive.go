package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

// ChapterArchiveResource describes one independently downloadable chapter.
type ChapterArchiveResource struct {
	ChapterNumber  int    `json:"chapter_number"`
	Title          string `json:"title"`
	FragmentsCount int    `json:"fragments_count"`
	DurationMS     int64  `json:"duration_ms"`
	AudioFilename  string `json:"audio_filename"`
	AudioZIPURL    string `json:"audio_zip_url"`
}

// JobChaptersResponse contains all chapter downloads available for a job.
type JobChaptersResponse struct {
	JobID       string                   `json:"job_id"`
	BookID      string                   `json:"book_id"`
	AudioZIPURL string                   `json:"audio_zip_url"`
	Chapters    []ChapterArchiveResource `json:"chapters"`
}

// listJobChapters handles GET /v1/job/{jobID}/chapters.
func (s *Server) listJobChapters(
	writer http.ResponseWriter,
	request *http.Request,
) {
	jobID := request.PathValue("jobID")
	catalog, err := s.store.jobChapterCatalog(request.Context(), jobID)
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
		s.logArchiveError(request, jobID, 0, err)
		writeProblem(
			writer,
			request,
			http.StatusInternalServerError,
			"ARCHIVE_ERROR",
			"could not prepare chapter downloads",
		)
		return
	}

	chapters, err := chapterArchiveResources(catalog.Chapters)
	if err != nil {
		s.logArchiveError(request, jobID, 0, err)
		writeProblem(
			writer,
			request,
			http.StatusInternalServerError,
			"ARCHIVE_ERROR",
			"could not prepare chapter downloads",
		)
		return
	}

	escapedJobID := url.PathEscape(jobID)
	for index := range chapters {
		chapters[index].AudioZIPURL = fmt.Sprintf(
			"/v1/job/%s/chapters/%d/audio.zip",
			escapedJobID,
			chapters[index].ChapterNumber,
		)
	}

	writeJSON(writer, http.StatusOK, JobChaptersResponse{
		JobID:       jobID,
		BookID:      catalog.BookID,
		AudioZIPURL: "/v1/job/" + escapedJobID + "/audio.zip",
		Chapters:    chapters,
	})
}

// getChapterAudioZIP handles
// GET /v1/job/{jobID}/chapters/{chapterNumber}/audio.zip.
func (s *Server) getChapterAudioZIP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	chapterNumber, err := positiveChapterNumber(
		request.PathValue("chapterNumber"),
	)
	if err != nil {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"INVALID_CHAPTER_NUMBER",
			"chapter number must be a positive integer",
		)
		return
	}

	jobID := request.PathValue("jobID")
	snapshot, err := s.store.chapterArchiveSnapshot(
		request.Context(),
		jobID,
		chapterNumber,
	)
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
	case errors.Is(err, errChapterNotFound):
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"CHAPTER_NOT_FOUND",
			"chapter not found",
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
		s.logArchiveError(request, jobID, chapterNumber, err)
		writeProblem(
			writer,
			request,
			http.StatusInternalServerError,
			"ARCHIVE_ERROR",
			"could not prepare the chapter audio archive",
		)
		return
	}

	archive, err := buildChapterAudioZIP(snapshot)
	if err != nil {
		s.logArchiveError(request, jobID, chapterNumber, err)
		writeProblem(
			writer,
			request,
			http.StatusInternalServerError,
			"ARCHIVE_ERROR",
			"could not prepare the chapter audio archive",
		)
		return
	}

	writer.Header().Set("Content-Type", "application/zip")
	writer.Header().Set(
		"Content-Disposition",
		fmt.Sprintf(
			`attachment; filename="ready-chapter-flac-%04d.zip"`,
			chapterNumber,
		),
	)
	writer.Header().Set("Content-Length", strconv.Itoa(len(archive)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(archive)
}

func chapterArchiveResources(
	summaries []chapterSummary,
) ([]ChapterArchiveResource, error) {
	summaries = slices.Clone(summaries)
	slices.SortFunc(summaries, func(left, right chapterSummary) int {
		return cmp.Compare(left.Number, right.Number)
	})

	chapters := make([]ChapterArchiveResource, 0, len(summaries))
	previousNumber := 0
	for _, summary := range summaries {
		if summary.Number <= 0 ||
			summary.FragmentsCount <= 0 ||
			summary.DurationMS < 0 {
			return nil, fmt.Errorf(
				"chapter %d has invalid catalog metadata",
				summary.Number,
			)
		}
		if summary.Number == previousNumber {
			return nil, fmt.Errorf(
				"chapter %d appears more than once in the catalog",
				summary.Number,
			)
		}
		chapters = append(chapters, ChapterArchiveResource{
			ChapterNumber:  summary.Number,
			Title:          summary.Title,
			FragmentsCount: summary.FragmentsCount,
			DurationMS:     summary.DurationMS,
			AudioFilename: chapterAudioFilename(
				summary.Number,
				summary.Title,
			),
		})
		previousNumber = summary.Number
	}

	return chapters, nil
}

func positiveChapterNumber(value string) (int, error) {
	if value == "" {
		return 0, errors.New("chapter number is empty")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("chapter number is not decimal")
		}
	}
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, errors.New("chapter number is not positive")
	}

	return number, nil
}

func (s *Server) logArchiveError(
	request *http.Request,
	jobID string,
	chapterNumber int,
	err error,
) {
	attributes := []any{
		"request_id", requestID(request.Context()),
		"job_id", jobID,
		"error", err,
	}
	if chapterNumber > 0 {
		attributes = append(attributes, "chapter_number", chapterNumber)
	}
	s.logger.Error("prepare chapter archive", attributes...)
}
