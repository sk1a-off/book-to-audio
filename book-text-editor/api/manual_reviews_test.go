package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"book-text-editor/internal/book"
)

func TestEncodeFragmentAudioWAVIsBoundedAndValidatesMetadata(t *testing.T) {
	t.Parallel()

	pcm := []byte{0, 0, 1, 0}
	wav, err := encodeFragmentAudioWAV(fragmentAudioSnapshot{
		PCM:         pcm,
		SampleRate:  24_000,
		Channels:    1,
		SampleWidth: 2,
		DurationMS:  1,
	})
	if err != nil {
		t.Fatalf("encodeFragmentAudioWAV() error = %v", err)
	}
	if len(wav) != 44+len(pcm) ||
		string(wav[:4]) != "RIFF" ||
		string(wav[8:12]) != "WAVE" ||
		!bytes.Equal(wav[44:], pcm) {
		t.Fatalf("WAV bytes are invalid: %x", wav)
	}

	tests := []struct {
		name  string
		audio fragmentAudioSnapshot
	}{
		{
			name: "empty payload",
			audio: fragmentAudioSnapshot{
				SampleRate: 24_000, Channels: 1, SampleWidth: 2,
			},
		},
		{
			name: "oversized payload",
			audio: fragmentAudioSnapshot{
				PCM:         make([]byte, maxFragmentAudioPCMBytes+1),
				SampleRate:  24_000,
				Channels:    1,
				SampleWidth: 2,
			},
		},
		{
			name: "unaligned payload",
			audio: fragmentAudioSnapshot{
				PCM:         []byte{1},
				SampleRate:  24_000,
				Channels:    1,
				SampleWidth: 2,
			},
		},
		{
			name: "invalid sample rate",
			audio: fragmentAudioSnapshot{
				PCM: []byte{0, 0}, Channels: 1, SampleWidth: 2,
			},
		},
		{
			name: "invalid channels",
			audio: fragmentAudioSnapshot{
				PCM: []byte{0, 0}, SampleRate: 24_000, SampleWidth: 2,
			},
		},
		{
			name: "unsupported sample width",
			audio: fragmentAudioSnapshot{
				PCM: []byte{0, 0}, SampleRate: 24_000, Channels: 1,
				SampleWidth: 1,
			},
		},
		{
			name: "negative duration",
			audio: fragmentAudioSnapshot{
				PCM:         []byte{0, 0},
				SampleRate:  24_000,
				Channels:    1,
				SampleWidth: 2,
				DurationMS:  -1,
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := encodeFragmentAudioWAV(test.audio); err == nil {
				t.Fatal("encodeFragmentAudioWAV() error = nil")
			}
		})
	}
}

func TestMemoryStoreApprovesWarningAndKeepsImmutableAudit(t *testing.T) {
	t.Parallel()

	store := manualReviewMemoryStore(FragmentStatusWarning, true)
	now := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	audioBefore, ok, err := store.fragmentAudio(
		context.Background(),
		"fragment-1",
	)
	if err != nil || !ok {
		t.Fatalf("fragmentAudio() = (%+v, %v, %v)", audioBefore, ok, err)
	}
	audioBefore.PCM[0] ^= 0xff
	audioAgain, _, _ := store.fragmentAudio(context.Background(), "fragment-1")
	if audioAgain.PCM[0] != 0 {
		t.Fatal("fragmentAudio() exposed mutable store bytes")
	}

	approval, err := store.approveFragment(
		context.Background(),
		"fragment-1",
		"review-1",
		"Прослушано вручную.",
		now,
	)
	if err != nil {
		t.Fatalf("approveFragment() error = %v", err)
	}
	if approval.Fragment.Status != FragmentStatusReady ||
		approval.Fragment.WarningCode != "" ||
		approval.Fragment.Error != "" ||
		approval.Fragment.STTText != "Распознанный текст." {
		t.Fatalf("approved fragment = %+v", approval.Fragment)
	}
	if approval.Job.Status != JobStatusCompleted ||
		approval.Job.FragmentsReady != 1 ||
		approval.Job.FragmentsWarnings != 0 {
		t.Fatalf("recomputed job = %+v", approval.Job)
	}
	if approval.Review.ID != "review-1" ||
		approval.Review.FragmentID != "fragment-1" ||
		approval.Review.Attempt != 2 ||
		approval.Review.Decision != FragmentManualReviewDecisionApproved ||
		approval.Review.WarningCode != "transcript_mismatch" ||
		approval.Review.STTText != "Распознанный текст." ||
		approval.Review.Reason != "Прослушано вручную." ||
		!approval.Review.CreatedAt.Equal(now) {
		t.Fatalf("manual review = %+v", approval.Review)
	}

	audioAfter, _, _ := store.fragmentAudio(context.Background(), "fragment-1")
	if !bytes.Equal(audioAfter.PCM, []byte{0, 0, 1, 0}) {
		t.Fatalf("audio changed after approval: %v", audioAfter.PCM)
	}
	reviews, ok, err := store.fragmentManualReviews(
		context.Background(),
		"fragment-1",
	)
	if err != nil || !ok || len(reviews) != 1 ||
		reviews[0] != approval.Review {
		t.Fatalf("fragmentManualReviews() = (%+v, %v, %v)", reviews, ok, err)
	}
	reviews[0].Reason = "mutated"
	storedReviews, _, _ := store.fragmentManualReviews(
		context.Background(),
		"fragment-1",
	)
	if storedReviews[0].Reason != "Прослушано вручную." {
		t.Fatal("fragmentManualReviews() exposed mutable store state")
	}

	if _, err := store.approveFragment(
		context.Background(),
		"fragment-1",
		"review-2",
		"",
		now.Add(time.Second),
	); !errors.Is(err, errConflict) {
		t.Fatalf("second approve error = %v, want errConflict", err)
	}
	storedReviews, _, _ = store.fragmentManualReviews(
		context.Background(),
		"fragment-1",
	)
	if len(storedReviews) != 1 {
		t.Fatalf("reviews after conflict = %+v", storedReviews)
	}
}

func TestMemoryStoreRejectsUnapprovableFragments(t *testing.T) {
	t.Parallel()

	for _, status := range []FragmentStatus{
		FragmentStatusPending,
		FragmentStatusGenerating,
		FragmentStatusReady,
		FragmentStatusFailed,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			store := manualReviewMemoryStore(status, true)
			if _, err := store.approveFragment(
				context.Background(),
				"fragment-1",
				"review-1",
				"",
				time.Now(),
			); !errors.Is(err, errConflict) {
				t.Fatalf("approveFragment(%s) error = %v", status, err)
			}
		})
	}

	t.Run("warning without playable audio", func(t *testing.T) {
		t.Parallel()
		store := manualReviewMemoryStore(FragmentStatusWarning, false)
		if _, err := store.approveFragment(
			context.Background(),
			"fragment-1",
			"review-1",
			"",
			time.Now(),
		); !errors.Is(err, errConflict) {
			t.Fatalf("approveFragment() error = %v", err)
		}
	})

	t.Run("missing fragment", func(t *testing.T) {
		t.Parallel()
		store := newMemoryStore()
		if _, err := store.approveFragment(
			context.Background(),
			"missing",
			"review-1",
			"",
			time.Now(),
		); !errors.Is(err, errNotFound) {
			t.Fatalf("approveFragment() error = %v", err)
		}
		if _, ok, err := store.fragmentAudio(
			context.Background(),
			"missing",
		); err != nil || ok {
			t.Fatalf("fragmentAudio(missing) = (_, %v, %v)", ok, err)
		}
		if reviews, ok, err := store.fragmentManualReviews(
			context.Background(),
			"missing",
		); err != nil || ok || reviews != nil {
			t.Fatalf(
				"fragmentManualReviews(missing) = (%+v, %v, %v)",
				reviews,
				ok,
				err,
			)
		}
	})
}

func TestManualReviewEndpointsPlayApproveAndAudit(t *testing.T) {
	_, handler, jobID, warning := endpointWarningFixture(t)

	audioBefore := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+warning.ID+"/audio.wav",
		nil,
		"",
	)
	if audioBefore.Code != http.StatusOK {
		t.Fatalf(
			"GET fragment audio status = %d, body = %s",
			audioBefore.Code,
			audioBefore.Body,
		)
	}
	if contentType := audioBefore.Header().Get("Content-Type"); contentType !=
		"audio/wav" {
		t.Errorf("audio Content-Type = %q", contentType)
	}
	if disposition := audioBefore.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "inline;") {
		t.Errorf("audio Content-Disposition = %q", disposition)
	}
	if audioBefore.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("audio Cache-Control = %q", audioBefore.Header().Get("Cache-Control"))
	}
	if audioBefore.Header().Get("X-Audio-Sample-Rate") != "24000" ||
		audioBefore.Header().Get("X-Audio-Channels") != "1" ||
		audioBefore.Header().Get("X-Audio-Sample-Width") != "2" {
		t.Errorf("audio metadata headers = %v", audioBefore.Header())
	}
	audioBytes := bytes.Clone(audioBefore.Body.Bytes())
	if len(audioBytes) != 44+16*2 ||
		string(audioBytes[:4]) != "RIFF" ||
		string(audioBytes[8:12]) != "WAVE" ||
		binary.LittleEndian.Uint32(audioBytes[40:44]) != 16*2 {
		t.Fatalf("audio WAV bytes are invalid: %x", audioBytes)
	}
	if length := audioBefore.Header().Get("Content-Length"); length !=
		strconv.Itoa(len(audioBytes)) {
		t.Errorf("audio Content-Length = %q", length)
	}

	approve := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/fragment/"+warning.ID+"/approve",
		strings.NewReader(`{"reason":"  Прослушано вручную.  "}`),
		"application/json",
	)
	if approve.Code != http.StatusCreated {
		t.Fatalf(
			"POST approve status = %d, body = %s",
			approve.Code,
			approve.Body,
		)
	}
	if location := approve.Header().Get("Location"); location !=
		"/v1/fragment/"+warning.ID+"/reviews" {
		t.Errorf("approve Location = %q", location)
	}
	var approved ApproveFragmentResponse
	endpointTestDecodeJSON(t, approve, &approved)
	if approved.Fragment.ID != warning.ID ||
		approved.Fragment.Status != FragmentStatusReady ||
		approved.Fragment.WarningCode != "" ||
		approved.Fragment.Error != "" ||
		approved.Fragment.STTText != warning.STTText {
		t.Fatalf("approve fragment = %+v", approved.Fragment)
	}
	if approved.Job.ID != jobID ||
		approved.Job.Status != JobStatusCompleted ||
		approved.Job.FragmentsReady != approved.Job.FragmentsCount ||
		approved.Job.FragmentsWarnings != 0 {
		t.Fatalf("approve job = %+v", approved.Job)
	}
	if approved.Review.ID == "" ||
		approved.Review.FragmentID != warning.ID ||
		approved.Review.Attempt != warning.Attempt ||
		approved.Review.Decision != FragmentManualReviewDecisionApproved ||
		approved.Review.WarningCode != warning.WarningCode ||
		approved.Review.STTText != warning.STTText ||
		approved.Review.Reason != "Прослушано вручную." {
		t.Fatalf("approve review = %+v", approved.Review)
	}

	reviewsResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+warning.ID+"/reviews",
		nil,
		"",
	)
	if reviewsResponse.Code != http.StatusOK {
		t.Fatalf(
			"GET reviews status = %d, body = %s",
			reviewsResponse.Code,
			reviewsResponse.Body,
		)
	}
	var reviews FragmentManualReviewsResponse
	endpointTestDecodeJSON(t, reviewsResponse, &reviews)
	if reviews.FragmentID != warning.ID ||
		len(reviews.Reviews) != 1 ||
		reviews.Reviews[0] != approved.Review {
		t.Fatalf("reviews response = %+v", reviews)
	}

	audioAfter := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+warning.ID+"/audio.wav",
		nil,
		"",
	)
	if audioAfter.Code != http.StatusOK ||
		!bytes.Equal(audioAfter.Body.Bytes(), audioBytes) {
		t.Fatalf("audio changed after approval: status=%d", audioAfter.Code)
	}
	job := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+jobID,
		nil,
		"",
	)
	var completed JobResource
	endpointTestDecodeJSON(t, job, &completed)
	if completed.Status != JobStatusCompleted {
		t.Fatalf("job after approval = %+v", completed)
	}
	warnings := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+jobID+"/warnings",
		nil,
		"",
	)
	var remaining WarningsResponse
	endpointTestDecodeJSON(t, warnings, &remaining)
	if remaining.Fragments == nil || len(remaining.Fragments) != 0 {
		t.Fatalf("warnings after approval = %+v", remaining)
	}

	secondApprove := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/fragment/"+warning.ID+"/approve",
		nil,
		"",
	)
	endpointTestAssertProblem(
		t,
		secondApprove,
		http.StatusConflict,
		"FRAGMENT_NOT_APPROVABLE",
	)
	reviewsResponse = endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/fragment/"+warning.ID+"/reviews",
		nil,
		"",
	)
	endpointTestDecodeJSON(t, reviewsResponse, &reviews)
	if len(reviews.Reviews) != 1 {
		t.Fatalf("reviews after conflict = %+v", reviews.Reviews)
	}

}

func TestManualReviewEndpointsValidateInputAndMissingResources(t *testing.T) {
	t.Run("strict and bounded approval JSON", func(t *testing.T) {
		_, handler, _, warning := endpointWarningFixture(t)

		unknown := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/fragment/"+warning.ID+"/approve",
			strings.NewReader(`{"unknown":true}`),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			unknown,
			http.StatusBadRequest,
			"INVALID_JSON",
		)

		longReasonBody, err := json.Marshal(ApproveFragmentRequest{
			Reason: strings.Repeat("я", maxManualReviewReasonLength+1),
		})
		if err != nil {
			t.Fatalf("marshal long reason: %v", err)
		}
		tooLong := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/fragment/"+warning.ID+"/approve",
			bytes.NewReader(longReasonBody),
			"application/json",
		)
		endpointTestAssertProblem(
			t,
			tooLong,
			http.StatusUnprocessableEntity,
			"REVIEW_REASON_TOO_LONG",
		)

		reviewsResponse := endpointTestRequest(
			t,
			handler,
			http.MethodGet,
			"/v1/fragment/"+warning.ID+"/reviews",
			nil,
			"",
		)
		var reviews FragmentManualReviewsResponse
		endpointTestDecodeJSON(t, reviewsResponse, &reviews)
		if reviews.Reviews == nil || len(reviews.Reviews) != 0 {
			t.Fatalf("reviews after invalid requests = %#v", reviews.Reviews)
		}
	})

	t.Run("optional empty body", func(t *testing.T) {
		_, handler, _, warning := endpointWarningFixture(t)
		response := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/fragment/"+warning.ID+"/approve",
			nil,
			"",
		)
		if response.Code != http.StatusCreated {
			t.Fatalf(
				"empty approve status = %d, body = %s",
				response.Code,
				response.Body,
			)
		}
		var approval ApproveFragmentResponse
		endpointTestDecodeJSON(t, response, &approval)
		if approval.Review.Reason != "" {
			t.Errorf("empty approval reason = %q", approval.Review.Reason)
		}
	})

	t.Run("missing fragment", func(t *testing.T) {
		fixture := newEndpointTestFixture(t)
		handler := fixture.server.Handler()
		for _, test := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/v1/fragment/missing/audio.wav"},
			{http.MethodPost, "/v1/fragment/missing/approve"},
			{http.MethodGet, "/v1/fragment/missing/reviews"},
		} {
			response := endpointTestRequest(
				t,
				handler,
				test.method,
				test.path,
				nil,
				"",
			)
			endpointTestAssertProblem(
				t,
				response,
				http.StatusNotFound,
				"FRAGMENT_NOT_FOUND",
			)
		}
	})

	t.Run("warning without playable audio", func(t *testing.T) {
		fixture := newEndpointTestFixture(t)
		store := fixture.server.store.(*memoryStore)
		store.mu.Lock()
		store.jobs["job-invalid-audio"] = &jobRecord{
			Resource: JobResource{
				ID:                "job-invalid-audio",
				Status:            JobStatusCompletedWithWarnings,
				FragmentsCount:    1,
				FragmentsWarnings: 1,
			},
			FragmentIDs: []string{"fragment-invalid-audio"},
		}
		store.fragments["fragment-invalid-audio"] = &fragmentRecord{
			Resource: FragmentResource{
				ID:          "fragment-invalid-audio",
				JobID:       "job-invalid-audio",
				Status:      FragmentStatusWarning,
				WarningCode: "audio_warning",
			},
		}
		store.mu.Unlock()

		handler := fixture.server.Handler()
		audio := endpointTestRequest(
			t,
			handler,
			http.MethodGet,
			"/v1/fragment/fragment-invalid-audio/audio.wav",
			nil,
			"",
		)
		endpointTestAssertProblem(
			t,
			audio,
			http.StatusConflict,
			"FRAGMENT_AUDIO_NOT_PLAYABLE",
		)
		approve := endpointTestRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/fragment/fragment-invalid-audio/approve",
			nil,
			"",
		)
		endpointTestAssertProblem(
			t,
			approve,
			http.StatusConflict,
			"FRAGMENT_NOT_APPROVABLE",
		)
	})
}

func TestPostgresManualReviewIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := preparePostgresStore(t, ctx, databaseURL)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

	createdBook, err := store.createBook(ctx, "review-book", book.Book{
		Title: "Review book",
		Chapters: []book.Chapter{{
			Number:   1,
			Title:    "Chapter",
			Segments: []string{"Warning fragment."},
		}},
	}, now)
	if err != nil {
		t.Fatalf("createBook() error = %v", err)
	}
	createdVoice, err := store.createVoice(
		ctx,
		"review-voice",
		"Review voice",
		"wav",
		"audio/wav",
		"Reference.",
		[]byte{0, 0},
		now,
	)
	if err != nil {
		t.Fatalf("createVoice() error = %v", err)
	}
	createdJob, task, err := store.createJob(
		ctx,
		"review-job",
		createdBook.ID,
		createdVoice.ID,
		[]string{"review-fragment"},
		[]string{"review-original-revision"},
		defaultGenerationSettings(),
		now,
	)
	if err != nil {
		t.Fatalf("createJob() error = %v", err)
	}
	if err := store.startTask(ctx, task, now.Add(time.Minute)); err != nil {
		t.Fatalf("startTask() error = %v", err)
	}
	if _, err := store.startFragment(
		ctx,
		"review-fragment",
		now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("startFragment() error = %v", err)
	}
	pcm := []byte{0, 0, 1, 0}
	if err := store.completeFragment(
		ctx,
		"review-fragment",
		testFragmentResult(
			pcm,
			"transcript_mismatch",
			"Different transcript.",
		),
		now.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("completeFragment() error = %v", err)
	}
	if err := store.finishTask(
		ctx,
		createdJob.ID,
		now.Add(4*time.Minute),
	); err != nil {
		t.Fatalf("finishTask() error = %v", err)
	}
	assertPostgresJobState(
		t,
		ctx,
		store,
		createdJob.ID,
		JobStatusCompletedWithWarnings,
		0,
		0,
		1,
		0,
	)

	audioBefore, ok, err := store.fragmentAudio(ctx, "review-fragment")
	if err != nil || !ok || !bytes.Equal(audioBefore.PCM, pcm) {
		t.Fatalf("fragmentAudio() = (%+v, %v, %v)", audioBefore, ok, err)
	}
	approvedAt := now.Add(5 * time.Minute)
	approval, err := store.approveFragment(
		ctx,
		"review-fragment",
		"manual-review-1",
		"Listened in full.",
		approvedAt,
	)
	if err != nil {
		t.Fatalf("approveFragment() error = %v", err)
	}
	if approval.Fragment.Status != FragmentStatusReady ||
		approval.Fragment.STTText != "Different transcript." ||
		approval.Fragment.WarningCode != "" ||
		approval.Job.Status != JobStatusCompleted ||
		approval.Job.FragmentsReady != 1 ||
		approval.Job.FragmentsWarnings != 0 {
		t.Fatalf("approval = %+v", approval)
	}
	if approval.Review.WarningCode != "transcript_mismatch" ||
		approval.Review.STTText != "Different transcript." ||
		approval.Review.Attempt != 1 ||
		!approval.Review.CreatedAt.Equal(approvedAt) {
		t.Fatalf("review snapshot = %+v", approval.Review)
	}

	audioAfter, ok, err := store.fragmentAudio(ctx, "review-fragment")
	if err != nil || !ok || !bytes.Equal(audioAfter.PCM, pcm) ||
		audioAfter.SampleRate != audioBefore.SampleRate ||
		audioAfter.Channels != audioBefore.Channels ||
		audioAfter.SampleWidth != audioBefore.SampleWidth {
		t.Fatalf("audio after approval = (%+v, %v, %v)", audioAfter, ok, err)
	}
	reviews, ok, err := store.fragmentManualReviews(ctx, "review-fragment")
	if err != nil || !ok || len(reviews) != 1 ||
		reviews[0] != approval.Review {
		t.Fatalf("fragmentManualReviews() = (%+v, %v, %v)", reviews, ok, err)
	}
	if _, err := store.approveFragment(
		ctx,
		"review-fragment",
		"manual-review-2",
		"",
		approvedAt.Add(time.Second),
	); !errors.Is(err, errConflict) {
		t.Fatalf("second approve error = %v", err)
	}
	var reviewCount int
	if err := store.pool.QueryRow(
		ctx,
		`SELECT count(*) FROM fragment_manual_reviews
		 WHERE fragment_id = $1`,
		"review-fragment",
	).Scan(&reviewCount); err != nil {
		t.Fatalf("count manual reviews: %v", err)
	}
	if reviewCount != 1 {
		t.Fatalf("manual review count = %d, want 1", reviewCount)
	}
	if _, ok, err := store.fragmentManualReviews(
		ctx,
		"missing",
	); err != nil || ok {
		t.Fatalf("fragmentManualReviews(missing) = (_, %v, %v)", ok, err)
	}
}

func manualReviewMemoryStore(
	status FragmentStatus,
	withAudio bool,
) *memoryStore {
	store := newMemoryStore()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	store.jobs["job-1"] = &jobRecord{
		Resource: JobResource{
			ID:                "job-1",
			Status:            JobStatusCompletedWithWarnings,
			FragmentsCount:    1,
			FragmentsWarnings: 1,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
		FragmentIDs: []string{"fragment-1"},
	}
	record := &fragmentRecord{
		Resource: FragmentResource{
			ID:          "fragment-1",
			JobID:       "job-1",
			Text:        "Исходный текст.",
			STTText:     "Распознанный текст.",
			Status:      status,
			WarningCode: "transcript_mismatch",
			Attempt:     2,
			UpdatedAt:   now,
		},
		SampleRate:  24_000,
		Channels:    1,
		SampleWidth: 2,
		DurationMS:  1,
	}
	if withAudio {
		record.AudioPCM = []byte{0, 0, 1, 0}
	}
	store.fragments["fragment-1"] = record
	return store
}

func endpointWarningFixture(
	t *testing.T,
) (endpointTestFixture, http.Handler, string, FragmentResource) {
	t.Helper()

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
			"POST book status = %d, body = %s",
			bookResponse.Code,
			bookResponse.Body,
		)
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
	if warningsResponse.Code != http.StatusOK {
		t.Fatalf(
			"GET warnings status = %d, body = %s",
			warningsResponse.Code,
			warningsResponse.Body,
		)
	}
	var warnings WarningsResponse
	endpointTestDecodeJSON(t, warningsResponse, &warnings)
	if len(warnings.Fragments) != 1 {
		t.Fatalf("warnings = %+v, want one fragment", warnings)
	}
	return fixture, handler, generation.JobID, warnings.Fragments[0]
}
