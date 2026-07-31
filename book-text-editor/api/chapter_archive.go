package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ChapterAudioResource describes both generation state and audio availability.
// A chapter can be downloadable while still containing review warnings.
type ChapterAudioResource struct {
	ChapterNumber    int    `json:"chapter_number"`
	Title            string `json:"title"`
	FragmentsCount   int    `json:"fragments_count"`
	FragmentsVoiced  int    `json:"fragments_voiced"`
	FragmentsReady   int    `json:"fragments_ready"`
	FragmentsPending int    `json:"fragments_pending"`
	FragmentsWarning int    `json:"fragments_warning"`
	FragmentsFailed  int    `json:"fragments_failed"`
	ReviewRequired   int    `json:"review_required"`
	DurationMS       int64  `json:"duration_ms"`
	AudioFilename    string `json:"audio_filename"`
	AudioURL         string `json:"audio_url,omitempty"`
	AudioZIPURL      string `json:"audio_zip_url,omitempty"`
	ReviewURL        string `json:"review_url"`
	AudioComplete    bool   `json:"audio_complete"`
	Ready            bool   `json:"ready"` // compatibility: means audio-complete
}

type ChapterArchiveResource = ChapterAudioResource

type JobChaptersResponse struct {
	JobID       string                 `json:"job_id"`
	BookID      string                 `json:"book_id"`
	AudioZIPURL string                 `json:"audio_zip_url,omitempty"`
	Chapters    []ChapterAudioResource `json:"chapters"`
	Fragments   []FragmentViewResource `json:"fragments,omitempty"`
}

type JobChapterResponse struct {
	JobID     string                 `json:"job_id"`
	BookID    string                 `json:"book_id"`
	Job       JobResource            `json:"job"`
	Chapter   ChapterAudioResource   `json:"chapter"`
	Fragments []FragmentViewResource `json:"fragments"`
}

func (s *Server) listJobChapters(writer http.ResponseWriter, request *http.Request) {
	jobID := request.PathValue("jobID")
	snapshot, found, err := s.fragments.catalog(request.Context(), jobID)
	switch {
	case err != nil:
		s.internalStoreError(writer, request, "list job fragments", err)
		return
	case !found:
		writeProblem(writer, request, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
		return
	}

	chapters, err := chapterAudioResources(jobID, snapshot.Fragments)
	if err != nil {
		s.logArchiveError(request, jobID, 0, err)
		writeProblem(writer, request, http.StatusInternalServerError,
			"CHAPTER_CATALOG_ERROR", "could not prepare chapter metadata")
		return
	}

	response := JobChaptersResponse{
		JobID:    jobID,
		BookID:   snapshot.Job.BookID,
		Chapters: chapters,
	}
	includeFragments := strings.TrimSpace(request.URL.Query().Get("include_fragments"))
	if includeFragments != "0" && !strings.EqualFold(includeFragments, "false") {
		response.Fragments = snapshot.Fragments
	}
	if chaptersAudioComplete(chapters) &&
		(snapshot.Job.Status == JobStatusCompleted ||
			snapshot.Job.Status == JobStatusCompletedWithWarnings) {
		response.AudioZIPURL = "/v1/job/" + url.PathEscape(jobID) + "/audio.zip"
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) getJobChapter(writer http.ResponseWriter, request *http.Request) {
	chapterNumber, err := positiveChapterNumber(request.PathValue("chapterNumber"))
	if err != nil {
		writeProblem(writer, request, http.StatusUnprocessableEntity,
			"INVALID_CHAPTER_NUMBER", "chapter number must be a positive integer")
		return
	}
	jobID := request.PathValue("jobID")
	snapshot, found, err := s.fragments.catalog(request.Context(), jobID)
	if err != nil {
		s.internalStoreError(writer, request, "get chapter fragments", err)
		return
	}
	if !found {
		writeProblem(writer, request, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
		return
	}
	allChapters, err := chapterAudioResources(jobID, snapshot.Fragments)
	if err != nil {
		s.internalStoreError(writer, request, "prepare chapter metadata", err)
		return
	}
	var chapter *ChapterAudioResource
	for index := range allChapters {
		if allChapters[index].ChapterNumber == chapterNumber {
			chapter = &allChapters[index]
			break
		}
	}
	if chapter == nil {
		writeProblem(writer, request, http.StatusNotFound, "CHAPTER_NOT_FOUND", "chapter not found")
		return
	}
	fragments := make([]FragmentViewResource, 0, chapter.FragmentsCount)
	for _, fragment := range snapshot.Fragments {
		if fragment.ChapterNumber == chapterNumber {
			fragments = append(fragments, fragment)
		}
	}
	writeJSON(writer, http.StatusOK, JobChapterResponse{
		JobID:     jobID,
		BookID:    snapshot.Job.BookID,
		Job:       snapshot.Job,
		Chapter:   *chapter,
		Fragments: fragments,
	})
}

// getChapterAudioFLAC returns the currently selected best audio for every
// fragment. Review warnings do not block export when all fragments have audio.
func (s *Server) getChapterAudioFLAC(writer http.ResponseWriter, request *http.Request) {
	chapterNumber, err := positiveChapterNumber(request.PathValue("chapterNumber"))
	if err != nil {
		writeProblem(writer, request, http.StatusUnprocessableEntity,
			"INVALID_CHAPTER_NUMBER", "chapter number must be a positive integer")
		return
	}

	jobID := request.PathValue("jobID")
	snapshot, err := s.fragments.chapterSnapshot(request.Context(), jobID, chapterNumber)
	switch {
	case errors.Is(err, errNotFound):
		writeProblem(writer, request, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
		return
	case errors.Is(err, errChapterNotFound):
		writeProblem(writer, request, http.StatusNotFound, "CHAPTER_NOT_FOUND", "chapter not found")
		return
	case errors.Is(err, errConflict):
		writeProblem(writer, request, http.StatusConflict, "CHAPTER_NOT_READY", err.Error())
		return
	case err != nil:
		s.logArchiveError(request, jobID, chapterNumber, err)
		writeProblem(writer, request, http.StatusInternalServerError,
			"CHAPTER_AUDIO_ERROR", "could not prepare chapter audio")
		return
	}

	audio, filename, err := buildChapterAudioFLAC(snapshot)
	if err != nil {
		s.logArchiveError(request, jobID, chapterNumber, err)
		writeProblem(writer, request, http.StatusInternalServerError,
			"CHAPTER_AUDIO_ERROR", "could not prepare chapter audio")
		return
	}

	if strings.HasSuffix(request.URL.Path, ".zip") {
		writer.Header().Set("Deprecation", "true")
		writer.Header().Set("Link", fmt.Sprintf(
			`</v1/job/%s/chapters/%d/audio.flac>; rel="successor-version"`,
			url.PathEscape(jobID), chapterNumber))
	}
	fallback := fmt.Sprintf("chapter-%04d.flac", chapterNumber)
	writer.Header().Set("Content-Type", "audio/flac")
	writer.Header().Set("Content-Disposition", fmt.Sprintf(
		`attachment; filename=%q; filename*=UTF-8''%s`,
		fallback, url.PathEscape(filename)))
	writer.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(audio)
}

func buildChapterAudioFLAC(snapshot archiveSnapshot) ([]byte, string, error) {
	chapters, _, err := encodeArchiveChapters(snapshot.Fragments)
	if err != nil {
		return nil, "", err
	}
	if len(chapters) != 1 {
		return nil, "", errors.New("chapter download contains multiple chapters")
	}
	return chapters[0].audioFLAC, chapters[0].manifest.Path, nil
}

func chapterAudioResources(jobID string, fragments []FragmentViewResource) ([]ChapterAudioResource, error) {
	if len(fragments) == 0 {
		return make([]ChapterAudioResource, 0), nil
	}
	fragments = slices.Clone(fragments)
	slices.SortFunc(fragments, func(left, right FragmentViewResource) int {
		if byChapter := cmp.Compare(left.ChapterNumber, right.ChapterNumber); byChapter != 0 {
			return byChapter
		}
		return cmp.Compare(left.Ordinal, right.Ordinal)
	})

	byNumber := make(map[int]*ChapterAudioResource)
	for _, fragment := range fragments {
		if fragment.ChapterNumber <= 0 || fragment.Ordinal <= 0 {
			return nil, errors.New("fragment catalog contains invalid ordering metadata")
		}
		chapter := byNumber[fragment.ChapterNumber]
		if chapter == nil {
			chapter = &ChapterAudioResource{
				ChapterNumber: fragment.ChapterNumber,
				Title:         fragment.ChapterTitle,
				AudioFilename: chapterAudioFilename(fragment.ChapterNumber, fragment.ChapterTitle),
				ReviewURL: fmt.Sprintf("/jobs/%s/chapters/%d",
					url.PathEscape(jobID), fragment.ChapterNumber),
			}
			byNumber[fragment.ChapterNumber] = chapter
		} else if chapter.Title != fragment.ChapterTitle {
			return nil, errors.New("fragment catalog contains inconsistent chapter titles")
		}

		chapter.FragmentsCount++
		if fragment.AudioAvailable {
			chapter.FragmentsVoiced++
			chapter.DurationMS += int64(fragment.DurationMS)
		}
		switch fragment.Status {
		case FragmentStatusReady:
			chapter.FragmentsReady++
		case FragmentStatusWarning:
			chapter.FragmentsWarning++
			chapter.ReviewRequired++
		case FragmentStatusFailed:
			chapter.FragmentsFailed++
			chapter.ReviewRequired++
		default:
			chapter.FragmentsPending++
		}
	}

	result := make([]ChapterAudioResource, 0, len(byNumber))
	for _, chapter := range byNumber {
		chapter.AudioComplete = chapter.FragmentsCount > 0 &&
			chapter.FragmentsVoiced == chapter.FragmentsCount
		chapter.Ready = chapter.AudioComplete
		if chapter.AudioComplete {
			escapedJobID := url.PathEscape(jobID)
			chapter.AudioURL = fmt.Sprintf(
				"/v1/job/%s/chapters/%d/audio.flac",
				escapedJobID, chapter.ChapterNumber)
			chapter.AudioZIPURL = fmt.Sprintf(
				"/v1/job/%s/chapters/%d/audio.zip",
				escapedJobID, chapter.ChapterNumber)
		}
		result = append(result, *chapter)
	}
	slices.SortFunc(result, func(left, right ChapterAudioResource) int {
		return cmp.Compare(left.ChapterNumber, right.ChapterNumber)
	})
	return result, nil
}

func chaptersAudioComplete(chapters []ChapterAudioResource) bool {
	if len(chapters) == 0 {
		return false
	}
	for _, chapter := range chapters {
		if !chapter.AudioComplete {
			return false
		}
	}
	return true
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

func (s *Server) logArchiveError(request *http.Request, jobID string, chapterNumber int, err error) {
	attributes := []any{
		"request_id", requestID(request.Context()),
		"job_id", jobID,
		"error", err,
	}
	if chapterNumber > 0 {
		attributes = append(attributes, "chapter_number", chapterNumber)
	}
	s.logger.Error("prepare chapter audio", attributes...)
}
