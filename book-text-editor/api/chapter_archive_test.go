package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

type chapterArchiveTestRepository struct {
	repository
	catalog     chapterCatalog
	catalogErr  error
	snapshot    archiveSnapshot
	snapshotErr error
}

func (r chapterArchiveTestRepository) jobChapterCatalog(
	context.Context,
	string,
) (chapterCatalog, error) {
	return r.catalog, r.catalogErr
}

func (r chapterArchiveTestRepository) chapterArchiveSnapshot(
	_ context.Context,
	_ string,
	chapterNumber int,
) (archiveSnapshot, error) {
	if r.snapshotErr != nil {
		return archiveSnapshot{}, r.snapshotErr
	}
	fragments := make([]archiveFragment, 0)
	for _, fragment := range r.snapshot.Fragments {
		if fragment.Resource.ChapterNumber == chapterNumber {
			fragments = append(fragments, fragment)
		}
	}
	if len(fragments) == 0 {
		return archiveSnapshot{}, fmtError(
			errChapterNotFound,
			"missing",
		)
	}
	r.snapshot.Fragments = fragments
	return r.snapshot, nil
}

func TestListJobChaptersReturnsSortedMetadata(t *testing.T) {
	server := chapterArchiveTestServer(chapterArchiveTestSnapshot())
	request := chapterArchiveTestRequest(
		"/v1/job/job-1/chapters",
		"job-1",
		"",
	)
	response := httptest.NewRecorder()

	server.listJobChapters(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"status = %d, body = %s",
			response.Code,
			response.Body,
		)
	}
	var payload JobChaptersResponse
	chapterArchiveTestDecodeJSON(t, response, &payload)
	if payload.JobID != "job-1" || payload.BookID != "book-1" {
		t.Errorf("response identifiers = %#v", payload)
	}
	if payload.AudioZIPURL != "/v1/job/job-1/audio.zip" {
		t.Errorf("audio_zip_url = %q", payload.AudioZIPURL)
	}
	want := []ChapterArchiveResource{
		{
			ChapterNumber:  1,
			Title:          "Начало",
			FragmentsCount: 2,
			DurationMS:     450,
			AudioFilename:  "character_0001_Начало.flac",
			AudioZIPURL:    "/v1/job/job-1/chapters/1/audio.zip",
		},
		{
			ChapterNumber:  2,
			Title:          "Продолжение",
			FragmentsCount: 1,
			DurationMS:     300,
			AudioFilename:  "character_0002_Продолжение.flac",
			AudioZIPURL:    "/v1/job/job-1/chapters/2/audio.zip",
		},
	}
	if !reflect.DeepEqual(payload.Chapters, want) {
		t.Errorf("chapters = %#v, want %#v", payload.Chapters, want)
	}
}

func TestGetChapterAudioZIPContainsOnlySelectedChapter(t *testing.T) {
	server := chapterArchiveTestServer(chapterArchiveTestSnapshot())
	request := chapterArchiveTestRequest(
		"/v1/job/job-1/chapters/1/audio.zip",
		"job-1",
		"1",
	)
	response := httptest.NewRecorder()

	server.getChapterAudioZIP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"status = %d, body = %s",
			response.Code,
			response.Body,
		)
	}
	if contentType := response.Header().Get("Content-Type"); contentType !=
		"application/zip" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if disposition := response.Header().Get("Content-Disposition"); disposition !=
		`attachment; filename="ready-chapter-flac-0001.zip"` {
		t.Errorf("Content-Disposition = %q", disposition)
	}

	reader, err := zip.NewReader(
		bytes.NewReader(response.Body.Bytes()),
		int64(response.Body.Len()),
	)
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		names = append(names, file.Name)
	}
	wantNames := []string{
		"character_0001_Начало.flac",
		"manifest.json",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("ZIP entries = %v, want %v", names, wantNames)
	}

	manifestFile := reader.File[len(reader.File)-1]
	manifestReader, err := manifestFile.Open()
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	manifestData, err := io.ReadAll(manifestReader)
	_ = manifestReader.Close()
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest chapterArchiveManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Version != chapterArchiveManifestVersion ||
		manifest.Scope != "chapter" ||
		manifest.Chapter.ChapterNumber != 1 ||
		manifest.Chapter.Title != "Начало" ||
		manifest.Chapter.FragmentsCount != 2 ||
		manifest.Chapter.DurationMS != 450 ||
		manifest.Chapter.Path != "character_0001_Начало.flac" ||
		manifest.Chapter.AudioFormat != "flac" ||
		manifest.Chapter.TotalSamples != 32 {
		t.Errorf("chapter manifest metadata = %+v", manifest)
	}
	if manifest.Job.FragmentsCount != 3 {
		t.Errorf(
			"manifest job fragments_count = %d, want full job count 3",
			manifest.Job.FragmentsCount,
		)
	}
	if len(manifest.Fragments) != 2 {
		t.Fatalf("manifest fragment count = %d", len(manifest.Fragments))
	}
	for _, fragment := range manifest.Fragments {
		if fragment.ChapterNumber != 1 ||
			fragment.Path != manifest.Chapter.Path {
			t.Errorf(
				"manifest fragment does not point to merged chapter: %+v",
				fragment,
			)
		}
	}
	assertArchiveTestFLAC(
		t,
		readChapterArchiveTestEntry(
			t,
			reader,
			manifest.Chapter.Path,
		),
		append(
			chapterArchiveTestPCM(1),
			chapterArchiveTestPCM(3)...,
		),
		32,
	)
}

func TestGetChapterAudioZIPIsDeterministic(t *testing.T) {
	server := chapterArchiveTestServer(chapterArchiveTestSnapshot())
	first := httptest.NewRecorder()
	second := httptest.NewRecorder()

	server.getChapterAudioZIP(
		first,
		chapterArchiveTestRequest("", "job-1", "1"),
	)
	server.getChapterAudioZIP(
		second,
		chapterArchiveTestRequest("", "job-1", "1"),
	)

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d", first.Code, second.Code)
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Error("chapter ZIP differs for the same snapshot")
	}
}

func TestChapterArchiveRejectsInvalidAndMissingChapter(t *testing.T) {
	server := chapterArchiveTestServer(chapterArchiveTestSnapshot())
	for _, value := range []string{"", "0", "-1", "+1", "one"} {
		value := value
		t.Run("invalid_"+value, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.getChapterAudioZIP(
				response,
				chapterArchiveTestRequest("", "job-1", value),
			)
			chapterArchiveTestAssertProblem(
				t,
				response,
				http.StatusUnprocessableEntity,
				"INVALID_CHAPTER_NUMBER",
			)
		})
	}

	response := httptest.NewRecorder()
	server.getChapterAudioZIP(
		response,
		chapterArchiveTestRequest("", "job-1", "99"),
	)
	chapterArchiveTestAssertProblem(
		t,
		response,
		http.StatusNotFound,
		"CHAPTER_NOT_FOUND",
	)
}

func TestChapterArchiveMapsRepositoryErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name:   "job not found",
			err:    fmtError(errNotFound, "missing"),
			status: http.StatusNotFound,
			code:   "JOB_NOT_FOUND",
		},
		{
			name:   "job not ready",
			err:    fmtError(errConflict, "not ready"),
			status: http.StatusConflict,
			code:   "JOB_NOT_READY",
		},
		{
			name:   "storage failure",
			err:    errors.New("database unavailable"),
			status: http.StatusInternalServerError,
			code:   "ARCHIVE_ERROR",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			server := chapterArchiveTestServerWithRepository(
				chapterArchiveTestRepository{catalogErr: test.err},
			)
			response := httptest.NewRecorder()
			server.listJobChapters(
				response,
				chapterArchiveTestRequest("", "job-1", ""),
			)
			chapterArchiveTestAssertProblem(
				t,
				response,
				test.status,
				test.code,
			)
		})
	}
}

func TestChapterAudioArchiveDistinguishesJobAndChapterErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name:   "job not found",
			err:    fmtError(errNotFound, "missing"),
			status: http.StatusNotFound,
			code:   "JOB_NOT_FOUND",
		},
		{
			name:   "chapter not found",
			err:    fmtError(errChapterNotFound, "missing"),
			status: http.StatusNotFound,
			code:   "CHAPTER_NOT_FOUND",
		},
		{
			name:   "job not ready",
			err:    fmtError(errConflict, "not ready"),
			status: http.StatusConflict,
			code:   "JOB_NOT_READY",
		},
		{
			name:   "storage failure",
			err:    errors.New("database unavailable"),
			status: http.StatusInternalServerError,
			code:   "ARCHIVE_ERROR",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			server := chapterArchiveTestServerWithRepository(
				chapterArchiveTestRepository{snapshotErr: test.err},
			)
			response := httptest.NewRecorder()
			server.getChapterAudioZIP(
				response,
				chapterArchiveTestRequest("", "job-1", "1"),
			)
			chapterArchiveTestAssertProblem(
				t,
				response,
				test.status,
				test.code,
			)
		})
	}
}

func chapterArchiveTestServer(snapshot archiveSnapshot) *Server {
	summaries, err := chapterArchiveTestSummaries(snapshot.Fragments)
	if err != nil {
		panic(err)
	}
	return chapterArchiveTestServerWithRepository(
		chapterArchiveTestRepository{
			catalog: chapterCatalog{
				BookID:   snapshot.Book.ID,
				Chapters: summaries,
			},
			snapshot: snapshot,
		},
	)
}

func chapterArchiveTestSummaries(
	fragments []archiveFragment,
) ([]chapterSummary, error) {
	byNumber := make(map[int][]archiveFragment)
	for _, fragment := range fragments {
		byNumber[fragment.Resource.ChapterNumber] = append(
			byNumber[fragment.Resource.ChapterNumber],
			fragment,
		)
	}
	summaries := make([]chapterSummary, 0, len(byNumber))
	for _, chapterFragments := range byNumber {
		summary, err := summarizeChapterArchive(chapterFragments)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func chapterArchiveTestServerWithRepository(
	store repository,
) *Server {
	return &Server{
		store: store,
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
	}
}

func chapterArchiveTestSnapshot() archiveSnapshot {
	updatedAt := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	return archiveSnapshot{
		Book: BookResource{ID: "book-1", Title: "Книга"},
		Voice: VoiceResource{
			ID:   "voice-1",
			Name: "Голос",
		},
		Job: JobResource{
			ID:             "job-1",
			BookID:         "book-1",
			VoiceID:        "voice-1",
			Status:         JobStatusCompleted,
			FragmentsCount: 3,
			FragmentsReady: 3,
			UpdatedAt:      updatedAt,
		},
		Fragments: []archiveFragment{
			chapterArchiveTestFragment(
				"fragment-2",
				2,
				2,
				"Продолжение",
				300,
				chapterArchiveTestPCM(2),
				updatedAt,
			),
			chapterArchiveTestFragment(
				"fragment-3",
				1,
				3,
				"Начало",
				250,
				chapterArchiveTestPCM(3),
				updatedAt,
			),
			chapterArchiveTestFragment(
				"fragment-1",
				1,
				1,
				"Начало",
				200,
				chapterArchiveTestPCM(1),
				updatedAt,
			),
		},
	}
}

func chapterArchiveTestPCM(base int16) []byte {
	const samples = 16
	result := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		binary.LittleEndian.PutUint16(
			result[index*2:index*2+2],
			uint16(base+int16(index)),
		)
	}
	return result
}

func readChapterArchiveTestEntry(
	t *testing.T,
	reader *zip.Reader,
	name string,
) []byte {
	t.Helper()
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			t.Fatalf("open ZIP entry %q: %v", name, err)
		}
		data, err := io.ReadAll(entry)
		_ = entry.Close()
		if err != nil {
			t.Fatalf("read ZIP entry %q: %v", name, err)
		}
		return data
	}
	t.Fatalf("ZIP entry %q not found", name)
	return nil
}

func chapterArchiveTestFragment(
	id string,
	chapterNumber, ordinal int,
	title string,
	durationMS int,
	pcm []byte,
	updatedAt time.Time,
) archiveFragment {
	return archiveFragment{
		Resource: FragmentResource{
			ID:            id,
			JobID:         "job-1",
			ChapterNumber: chapterNumber,
			Ordinal:       ordinal,
			Text:          "Текст",
			STTText:       "Текст",
			Status:        FragmentStatusReady,
			Attempt:       1,
			UpdatedAt:     updatedAt,
		},
		ChapterTitle: title,
		AudioPCM:     bytes.Clone(pcm),
		SampleRate:   24_000,
		Channels:     1,
		SampleWidth:  2,
		DurationMS:   durationMS,
	}
}

func chapterArchiveTestRequest(
	target, jobID, chapterNumber string,
) *http.Request {
	if target == "" {
		target = "/"
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.SetPathValue("jobID", jobID)
	request.SetPathValue("chapterNumber", chapterNumber)
	return request
}

func chapterArchiveTestDecodeJSON(
	t *testing.T,
	response *httptest.ResponseRecorder,
	target any,
) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode JSON: %v; body = %s", err, response.Body)
	}
}

func chapterArchiveTestAssertProblem(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	code string,
) {
	t.Helper()
	if response.Code != status {
		t.Fatalf(
			"status = %d, want %d; body = %s",
			response.Code,
			status,
			response.Body,
		)
	}
	var problem ErrorResponse
	chapterArchiveTestDecodeJSON(t, response, &problem)
	if problem.Code != code {
		t.Errorf("problem code = %q, want %q", problem.Code, code)
	}
}

func fmtError(kind error, detail string) error {
	return &chapterArchiveTestError{kind: kind, detail: detail}
}

type chapterArchiveTestError struct {
	kind   error
	detail string
}

func (e *chapterArchiveTestError) Error() string {
	return e.kind.Error() + ": " + e.detail
}

func (e *chapterArchiveTestError) Unwrap() error {
	return e.kind
}
