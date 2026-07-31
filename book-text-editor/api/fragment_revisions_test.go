package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"book-text-editor/internal/book"
)

func TestMemoryStoreFragmentTextRevisionHistoryIsImmutable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newMemoryStore()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	task := createMemoryRevisionFixture(t, ctx, store, now)

	history, ok, err := store.fragmentRevisions(ctx, "fragment-1")
	if err != nil || !ok {
		t.Fatalf("fragmentRevisions(original) = (%+v, %v, %v)", history, ok, err)
	}
	assertRevisionHistory(
		t,
		history,
		"original-revision",
		revisionExpectation{
			id:     "original-revision",
			number: 1,
			source: FragmentTextRevisionSourceOriginal,
			text:   "Исходный текст.",
		},
	)

	if err := store.startTask(ctx, task, now.Add(time.Minute)); err != nil {
		t.Fatalf("startTask() error = %v", err)
	}
	if _, err := store.startFragment(
		ctx,
		"fragment-1",
		now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("startFragment() error = %v", err)
	}
	if err := store.failFragment(
		ctx,
		"fragment-1",
		"test failure",
		now.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("failFragment() error = %v", err)
	}

	edited, err := store.editFragment(
		ctx,
		"fragment-1",
		"Исправленный текст.",
		"manual-revision",
		now.Add(4*time.Minute),
	)
	if err != nil {
		t.Fatalf("editFragment() error = %v", err)
	}
	if edited.Text != "Исправленный текст." ||
		edited.Status != FragmentStatusWarning ||
		edited.WarningCode != "text_edited" {
		t.Fatalf("editFragment() = %+v", edited)
	}

	history, ok, err = store.fragmentRevisions(ctx, "fragment-1")
	if err != nil || !ok {
		t.Fatalf("fragmentRevisions(manual) = (%+v, %v, %v)", history, ok, err)
	}
	assertRevisionHistory(
		t,
		history,
		"manual-revision",
		revisionExpectation{
			id:     "original-revision",
			number: 1,
			source: FragmentTextRevisionSourceOriginal,
			text:   "Исходный текст.",
		},
		revisionExpectation{
			id:       "manual-revision",
			number:   2,
			source:   FragmentTextRevisionSourceManual,
			text:     "Исправленный текст.",
			parentID: "original-revision",
		},
	)

	_, err = store.restoreFragmentRevision(
		ctx,
		"fragment-1",
		"missing-revision",
		"unused-restore-revision",
		"",
		now.Add(5*time.Minute),
	)
	if !errors.Is(err, errRevisionNotFound) {
		t.Fatalf("restore missing revision error = %v, want errRevisionNotFound", err)
	}

	mutation, err := store.restoreFragmentRevision(
		ctx,
		"fragment-1",
		"original-revision",
		"restore-revision",
		"Вернуть авторский вариант",
		now.Add(6*time.Minute),
	)
	if err != nil {
		t.Fatalf("restoreFragmentRevision() error = %v", err)
	}
	if mutation.Fragment.Text != "Исходный текст." ||
		mutation.Fragment.Status != FragmentStatusWarning ||
		mutation.Fragment.WarningCode != "text_restored" {
		t.Fatalf("restored fragment = %+v", mutation.Fragment)
	}
	if mutation.Revision.ID != "restore-revision" ||
		mutation.Revision.RevisionNumber != 3 ||
		mutation.Revision.Source != FragmentTextRevisionSourceRestore ||
		mutation.Revision.ParentRevisionID == nil ||
		*mutation.Revision.ParentRevisionID != "original-revision" ||
		mutation.Revision.Reason == nil ||
		*mutation.Revision.Reason != "Вернуть авторский вариант" {
		t.Fatalf("restore revision = %+v", mutation.Revision)
	}

	history, ok, err = store.fragmentRevisions(ctx, "fragment-1")
	if err != nil || !ok {
		t.Fatalf("fragmentRevisions(restored) = (%+v, %v, %v)", history, ok, err)
	}
	assertRevisionHistory(
		t,
		history,
		"restore-revision",
		revisionExpectation{
			id:     "original-revision",
			number: 1,
			source: FragmentTextRevisionSourceOriginal,
			text:   "Исходный текст.",
		},
		revisionExpectation{
			id:       "manual-revision",
			number:   2,
			source:   FragmentTextRevisionSourceManual,
			text:     "Исправленный текст.",
			parentID: "original-revision",
		},
		revisionExpectation{
			id:       "restore-revision",
			number:   3,
			source:   FragmentTextRevisionSourceRestore,
			text:     "Исходный текст.",
			parentID: "original-revision",
			reason:   "Вернуть авторский вариант",
		},
	)

	// Mutating a returned DTO must not mutate the stored immutable history.
	history.Revisions[0].Text = "Подмена"
	*history.Revisions[2].Reason = "Подмена причины"
	unchanged, ok, err := store.fragmentRevisions(ctx, "fragment-1")
	if err != nil || !ok {
		t.Fatalf("fragmentRevisions(after DTO mutation) = (%+v, %v, %v)", unchanged, ok, err)
	}
	if unchanged.Revisions[0].Text != "Исходный текст." ||
		unchanged.Revisions[2].Reason == nil ||
		*unchanged.Revisions[2].Reason != "Вернуть авторский вариант" {
		t.Fatalf("stored revision history was mutated through DTO: %+v", unchanged)
	}

	retry, err := store.prepareRetry(
		ctx,
		"job-1",
		[]string{"fragment-1"},
		now.Add(7*time.Minute),
	)
	if err != nil {
		t.Fatalf("prepareRetry() error = %v", err)
	}
	if err := store.startTask(ctx, retry, now.Add(8*time.Minute)); err != nil {
		t.Fatalf("start retry task error = %v", err)
	}
	if _, err := store.startFragment(
		ctx,
		"fragment-1",
		now.Add(9*time.Minute),
	); err != nil {
		t.Fatalf("start retry fragment error = %v", err)
	}
	_, err = store.restoreFragmentRevision(
		ctx,
		"fragment-1",
		"original-revision",
		"busy-restore-revision",
		"",
		now.Add(10*time.Minute),
	)
	if !errors.Is(err, errConflict) {
		t.Fatalf("restore generating fragment error = %v, want errConflict", err)
	}
}

func TestMemoryStoreRestorePendingFragmentPreservesQueuedJob(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newMemoryStore()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	createMemoryRevisionFixture(t, ctx, store, now)

	mutation, err := store.restoreFragmentRevision(
		ctx,
		"fragment-1",
		"original-revision",
		"pending-restore",
		"",
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("restoreFragmentRevision() error = %v", err)
	}
	if mutation.Fragment.Status != FragmentStatusPending ||
		mutation.Fragment.WarningCode != "" {
		t.Fatalf("restored pending fragment = %+v", mutation.Fragment)
	}
	job, ok, err := store.job(ctx, "job-1")
	if err != nil || !ok {
		t.Fatalf("job() = (%+v, %v, %v)", job, ok, err)
	}
	if job.Status != JobStatusQueued || job.FragmentsPending != 1 {
		t.Fatalf("job after pending restore = %+v", job)
	}
	history, ok, err := store.fragmentRevisions(ctx, "fragment-1")
	if err != nil || !ok {
		t.Fatalf("fragmentRevisions() = (%+v, %v, %v)", history, ok, err)
	}
	if history.CurrentRevisionID != "pending-restore" ||
		len(history.Revisions) != 2 ||
		history.Revisions[1].Source != FragmentTextRevisionSourceRestore {
		t.Fatalf("pending restore history = %+v", history)
	}
}

func TestFragmentRevisionEndpoints(t *testing.T) {
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
		t.Fatalf("POST book status = %d, body = %s", bookResponse.Code, bookResponse.Body)
	}
	var bookResource BookResource
	endpointTestDecodeJSON(t, bookResponse, &bookResource)

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
			map[string]string{
				"name":           "Revision voice",
				"reference_text": "Эталонная фраза.",
			},
		),
	)
	if voiceResponse.Code != http.StatusCreated {
		t.Fatalf("POST voice status = %d, body = %s", voiceResponse.Code, voiceResponse.Body)
	}
	var voiceResource VoiceResource
	endpointTestDecodeJSON(t, voiceResponse, &voiceResource)

	generationResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/generate/book/"+bookResource.ID+"/voice/"+voiceResource.ID,
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
	endpointTestWaitForJob(
		t,
		handler,
		generation.JobID,
		JobStatusCompletedWithWarnings,
	)

	warningsResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/warnings",
		nil,
		"",
	)
	var warnings WarningsResponse
	endpointTestDecodeJSON(t, warningsResponse, &warnings)
	if len(warnings.Fragments) != 1 {
		t.Fatalf("warnings = %+v, want one fragment", warnings)
	}
	fragment := warnings.Fragments[0]

	originalResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+fragment.ID+"/revisions",
		nil,
		"",
	)
	if originalResponse.Code != http.StatusOK {
		t.Fatalf(
			"GET original revisions status = %d, body = %s",
			originalResponse.Code,
			originalResponse.Body,
		)
	}
	var originalHistory FragmentRevisionsResponse
	endpointTestDecodeJSON(t, originalResponse, &originalHistory)
	if originalHistory.FragmentID != fragment.ID ||
		len(originalHistory.Revisions) != 1 ||
		originalHistory.Revisions[0].Source != FragmentTextRevisionSourceOriginal ||
		originalHistory.CurrentRevisionID != originalHistory.Revisions[0].ID {
		t.Fatalf("original revision history = %+v", originalHistory)
	}
	originalRevision := originalHistory.Revisions[0]

	editResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPatch,
		"/v1/fragment/"+fragment.ID,
		strings.NewReader(`{"new_text":"Исправленный вариант."}`),
		"application/json",
	)
	if editResponse.Code != http.StatusOK {
		t.Fatalf("PATCH fragment status = %d, body = %s", editResponse.Code, editResponse.Body)
	}

	manualResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+fragment.ID+"/revisions",
		nil,
		"",
	)
	var manualHistory FragmentRevisionsResponse
	endpointTestDecodeJSON(t, manualResponse, &manualHistory)
	if len(manualHistory.Revisions) != 2 {
		t.Fatalf("manual revision history = %+v", manualHistory)
	}
	manualRevision := manualHistory.Revisions[1]
	if manualRevision.Source != FragmentTextRevisionSourceManual ||
		manualRevision.RevisionNumber != 2 ||
		manualRevision.Text != "Исправленный вариант." ||
		manualRevision.ParentRevisionID == nil ||
		*manualRevision.ParentRevisionID != originalRevision.ID ||
		manualHistory.CurrentRevisionID != manualRevision.ID {
		t.Fatalf("manual revision = %+v, history = %+v", manualRevision, manualHistory)
	}

	restorePath := "/v1/fragment/" + fragment.ID +
		"/revisions/" + originalRevision.ID + "/restore"
	restoreResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		restorePath,
		strings.NewReader(`{"reason":"Вернуть исходный текст"}`),
		"application/json",
	)
	if restoreResponse.Code != http.StatusCreated {
		t.Fatalf(
			"POST restore status = %d, body = %s",
			restoreResponse.Code,
			restoreResponse.Body,
		)
	}
	if location := restoreResponse.Header().Get("Location"); location !=
		"/v1/fragment/"+fragment.ID+"/revisions" {
		t.Errorf("restore Location = %q", location)
	}
	var restored RestoreFragmentRevisionResponse
	endpointTestDecodeJSON(t, restoreResponse, &restored)
	if restored.Fragment.Text != originalRevision.Text ||
		restored.Fragment.Status != FragmentStatusWarning ||
		restored.Fragment.WarningCode != "text_restored" ||
		restored.Revision.Source != FragmentTextRevisionSourceRestore ||
		restored.Revision.RevisionNumber != 3 ||
		restored.Revision.ParentRevisionID == nil ||
		*restored.Revision.ParentRevisionID != originalRevision.ID ||
		restored.Revision.Reason == nil ||
		*restored.Revision.Reason != "Вернуть исходный текст" {
		t.Fatalf("restore response = %+v", restored)
	}

	finalResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+fragment.ID+"/revisions",
		nil,
		"",
	)
	var finalHistory FragmentRevisionsResponse
	endpointTestDecodeJSON(t, finalResponse, &finalHistory)
	if len(finalHistory.Revisions) != 3 ||
		finalHistory.CurrentRevisionID != restored.Revision.ID ||
		finalHistory.Revisions[0].Text != originalRevision.Text ||
		finalHistory.Revisions[1].Text != "Исправленный вариант." {
		t.Fatalf("final immutable history = %+v", finalHistory)
	}

	missingFragment := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/missing/revisions",
		nil,
		"",
	)
	endpointTestAssertProblem(
		t,
		missingFragment,
		http.StatusNotFound,
		"FRAGMENT_NOT_FOUND",
	)

	missingRevision := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/fragment/"+fragment.ID+"/revisions/missing/restore",
		nil,
		"",
	)
	endpointTestAssertProblem(
		t,
		missingRevision,
		http.StatusNotFound,
		"REVISION_NOT_FOUND",
	)

	unknownField := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		restorePath,
		strings.NewReader(`{"comment":"not allowed"}`),
		"application/json",
	)
	endpointTestAssertProblem(
		t,
		unknownField,
		http.StatusBadRequest,
		"INVALID_JSON",
	)

	longReason := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		restorePath,
		strings.NewReader(
			fmt.Sprintf(`{"reason":%q}`, strings.Repeat("я", maxRevisionReasonLength+1)),
		),
		"application/json",
	)
	endpointTestAssertProblem(
		t,
		longReason,
		http.StatusUnprocessableEntity,
		"REVISION_REASON_TOO_LONG",
	)
}

func TestPostgresFragmentTextRevisionsIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := preparePostgresStore(t, ctx, databaseURL)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

	createdBook, err := store.createBook(
		ctx,
		"revision-book",
		book.Book{
			Title: "Revision book",
			Chapters: []book.Chapter{
				{
					Number:   1,
					Title:    "Chapter",
					Segments: []string{"Исходный текст."},
				},
			},
		},
		now,
	)
	if err != nil {
		t.Fatalf("createBook() error = %v", err)
	}
	createdVoice, err := store.createVoice(
		ctx,
		"revision-voice",
		"Revision voice",
		"wav",
		"audio/wav",
		"Эталон.",
		[]byte("voice"),
		now,
	)
	if err != nil {
		t.Fatalf("createVoice() error = %v", err)
	}
	_, task, err := store.createJob(
		ctx,
		"revision-job",
		createdBook.ID,
		createdVoice.ID,
		[]string{"revision-fragment"},
		[]string{"revision-original"},
		defaultGenerationSettings(),
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("createJob() error = %v", err)
	}

	history, ok, err := store.fragmentRevisions(ctx, "revision-fragment")
	if err != nil || !ok {
		t.Fatalf("fragmentRevisions(original) = (%+v, %v, %v)", history, ok, err)
	}
	assertRevisionHistory(
		t,
		history,
		"revision-original",
		revisionExpectation{
			id:     "revision-original",
			number: 1,
			source: FragmentTextRevisionSourceOriginal,
			text:   "Исходный текст.",
		},
	)

	if err := store.startTask(ctx, task, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("startTask() error = %v", err)
	}
	if _, err := store.startFragment(
		ctx,
		"revision-fragment",
		now.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("startFragment() error = %v", err)
	}
	if err := store.failFragment(
		ctx,
		"revision-fragment",
		"test failure",
		now.Add(4*time.Minute),
	); err != nil {
		t.Fatalf("failFragment() error = %v", err)
	}
	if _, err := store.editFragment(
		ctx,
		"revision-fragment",
		"Ручная правка.",
		"revision-manual",
		now.Add(5*time.Minute),
	); err != nil {
		t.Fatalf("editFragment() error = %v", err)
	}

	mutation, err := store.restoreFragmentRevision(
		ctx,
		"revision-fragment",
		"revision-original",
		"revision-restore",
		"Откат для проверки",
		now.Add(6*time.Minute),
	)
	if err != nil {
		t.Fatalf("restoreFragmentRevision() error = %v", err)
	}
	if mutation.Fragment.Text != "Исходный текст." ||
		mutation.Fragment.Status != FragmentStatusWarning ||
		mutation.Revision.RevisionNumber != 3 {
		t.Fatalf("restore mutation = %+v", mutation)
	}

	history, ok, err = store.fragmentRevisions(ctx, "revision-fragment")
	if err != nil || !ok {
		t.Fatalf("fragmentRevisions(restored) = (%+v, %v, %v)", history, ok, err)
	}
	assertRevisionHistory(
		t,
		history,
		"revision-restore",
		revisionExpectation{
			id:     "revision-original",
			number: 1,
			source: FragmentTextRevisionSourceOriginal,
			text:   "Исходный текст.",
		},
		revisionExpectation{
			id:       "revision-manual",
			number:   2,
			source:   FragmentTextRevisionSourceManual,
			text:     "Ручная правка.",
			parentID: "revision-original",
		},
		revisionExpectation{
			id:       "revision-restore",
			number:   3,
			source:   FragmentTextRevisionSourceRestore,
			text:     "Исходный текст.",
			parentID: "revision-original",
			reason:   "Откат для проверки",
		},
	)

	var persistedText string
	if err := store.pool.QueryRow(
		ctx,
		`SELECT text FROM job_fragments WHERE id = $1`,
		"revision-fragment",
	).Scan(&persistedText); err != nil {
		t.Fatalf("query restored fragment text: %v", err)
	}
	if persistedText != "Исходный текст." {
		t.Fatalf("persisted current text = %q", persistedText)
	}

	_, err = store.restoreFragmentRevision(
		ctx,
		"revision-fragment",
		"missing",
		"revision-unused",
		"",
		now.Add(7*time.Minute),
	)
	if !errors.Is(err, errRevisionNotFound) {
		t.Fatalf("restore missing revision error = %v, want errRevisionNotFound", err)
	}
}

type revisionExpectation struct {
	id       string
	number   int
	source   FragmentTextRevisionSource
	text     string
	parentID string
	reason   string
}

func assertRevisionHistory(
	t *testing.T,
	history fragmentRevisionHistory,
	currentID string,
	want ...revisionExpectation,
) {
	t.Helper()

	if history.CurrentRevisionID != currentID {
		t.Errorf(
			"current revision id = %q, want %q",
			history.CurrentRevisionID,
			currentID,
		)
	}
	if len(history.Revisions) != len(want) {
		t.Fatalf(
			"revision count = %d, want %d; history = %+v",
			len(history.Revisions),
			len(want),
			history,
		)
	}
	for index, expectation := range want {
		revision := history.Revisions[index]
		if revision.ID != expectation.id ||
			revision.RevisionNumber != expectation.number ||
			revision.Source != expectation.source ||
			revision.Text != expectation.text ||
			!optionalStringEqual(revision.ParentRevisionID, expectation.parentID) ||
			!optionalStringEqual(revision.Reason, expectation.reason) ||
			revision.ModelID != nil ||
			revision.Prompt != nil {
			t.Errorf(
				"revision %d = %+v, want %+v",
				index,
				revision,
				expectation,
			)
		}
	}
}

func optionalStringEqual(value *string, want string) bool {
	if want == "" {
		return value == nil
	}
	return value != nil && *value == want
}

func createMemoryRevisionFixture(
	t *testing.T,
	ctx context.Context,
	store *memoryStore,
	now time.Time,
) jobTask {
	t.Helper()

	createdBook, err := store.createBook(
		ctx,
		"book-1",
		book.Book{
			Title: "Revision book",
			Chapters: []book.Chapter{
				{
					Number:   1,
					Title:    "Chapter",
					Segments: []string{"Исходный текст."},
				},
			},
		},
		now,
	)
	if err != nil {
		t.Fatalf("createBook() error = %v", err)
	}
	createdVoice, err := store.createVoice(
		ctx,
		"voice-1",
		"Revision voice",
		"wav",
		"audio/wav",
		"Эталон.",
		[]byte("voice"),
		now,
	)
	if err != nil {
		t.Fatalf("createVoice() error = %v", err)
	}
	_, task, err := store.createJob(
		ctx,
		"job-1",
		createdBook.ID,
		createdVoice.ID,
		[]string{"fragment-1"},
		[]string{"original-revision"},
		defaultGenerationSettings(),
		now,
	)
	if err != nil {
		t.Fatalf("createJob() error = %v", err)
	}
	return task
}

func TestCloneRevisionHistoryPointers(t *testing.T) {
	t.Parallel()

	parent := "parent"
	reason := "reason"
	input := []FragmentTextRevisionResource{
		{
			ID:               "revision",
			ParentRevisionID: &parent,
			Reason:           &reason,
		},
	}
	cloned := cloneTextRevisions(input)
	if !reflect.DeepEqual(cloned, input) {
		t.Fatalf("cloneTextRevisions() = %+v, want %+v", cloned, input)
	}
	*cloned[0].ParentRevisionID = "changed"
	*cloned[0].Reason = "changed"
	if *input[0].ParentRevisionID != "parent" || *input[0].Reason != "reason" {
		t.Fatal("cloneTextRevisions() retained mutable pointer aliases")
	}
}
