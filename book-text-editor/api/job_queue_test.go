package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"book-text-editor/internal/book"
)

func TestMemoryGenerationQueueRecoveryControlsAndFIFO(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	for index, id := range []string{"book-a", "book-b", "book-legacy", "book-paused"} {
		store.books[id] = &bookRecord{Resource: BookResource{
			ID:    id,
			Title: fmt.Sprintf("Book %d", index+1),
		}}
	}
	store.jobs["job-a"] = queueTestJob(
		"job-a", "book-a", JobStatusRunning, now, "a-1", "a-2",
	)
	store.jobs["job-b"] = queueTestJob(
		"job-b", "book-b", JobStatusQueued, now.Add(time.Minute), "b-1",
	)
	store.jobs["job-legacy"] = queueTestJob(
		"job-legacy", "book-legacy", JobStatusFailed, now.Add(2*time.Minute), "legacy-1",
	)
	store.jobs["job-paused"] = queueTestJob(
		"job-paused", "book-paused", JobStatusPaused, now.Add(3*time.Minute), "paused-1",
	)
	store.fragments["a-1"] = queueTestFragment("a-1", "job-a", FragmentStatusGenerating, 1)
	store.fragments["a-2"] = queueTestFragment("a-2", "job-a", FragmentStatusPending, 2)
	store.fragments["b-1"] = queueTestFragment("b-1", "job-b", FragmentStatusPending, 1)
	legacy := queueTestFragment("legacy-1", "job-legacy", FragmentStatusFailed, 1)
	legacy.Resource.Error = "generation interrupted"
	store.fragments["legacy-1"] = legacy
	store.fragments["paused-1"] = queueTestFragment(
		"paused-1", "job-paused", FragmentStatusPending, 1,
	)

	if err := store.recoverGenerationQueue(context.Background(), now.Add(4*time.Minute)); err != nil {
		t.Fatalf("recoverGenerationQueue() error = %v", err)
	}
	if got := store.jobs["job-a"].Resource.Status; got != JobStatusQueued {
		t.Fatalf("recovered running job status = %q, want queued", got)
	}
	if got := store.fragments["a-1"].Resource.Status; got != FragmentStatusPending {
		t.Fatalf("recovered generating fragment status = %q, want pending", got)
	}
	if got := store.jobs["job-legacy"].Resource.Status; got != JobStatusQueued {
		t.Fatalf("legacy interrupted job status = %q, want queued", got)
	}
	if fragment := store.fragments["legacy-1"].Resource; fragment.Status != FragmentStatusPending || fragment.Error != "" {
		t.Fatalf("legacy interrupted fragment = %+v, want clean pending", fragment)
	}

	snapshot, err := store.generationQueue(context.Background())
	if err != nil {
		t.Fatalf("generationQueue() error = %v", err)
	}
	if len(snapshot.Jobs) != 4 || len(snapshot.Fragments) != 5 {
		t.Fatalf("queue snapshot = %+v", snapshot)
	}
	if snapshot.Jobs[0].Job.ID != "job-a" ||
		snapshot.Jobs[1].Job.ID != "job-b" ||
		snapshot.Jobs[2].Job.ID != "job-legacy" ||
		snapshot.Jobs[3].Job.ID != "job-paused" {
		t.Fatalf("queue job order = %#v", snapshot.Jobs)
	}

	task, ok, err := store.claimGenerationTask(context.Background(), now.Add(5*time.Minute))
	if err != nil || !ok || task.JobID != "job-a" || !task.Claimed || len(task.FragmentIDs) != 2 {
		t.Fatalf("claimGenerationTask() = (%+v, %v, %v)", task, ok, err)
	}
	if _, err := store.startFragment(context.Background(), "a-1", now.Add(6*time.Minute)); err != nil {
		t.Fatalf("startFragment() error = %v", err)
	}
	paused, err := store.pauseJob(context.Background(), "job-a", now.Add(7*time.Minute))
	if err != nil || paused.Status != JobStatusPaused {
		t.Fatalf("pauseJob() = (%+v, %v)", paused, err)
	}
	if got := store.fragments["a-1"].Resource.Status; got != FragmentStatusPending {
		t.Fatalf("paused active fragment status = %q, want pending", got)
	}
	if _, err := store.startFragment(
		context.Background(), "a-1", now.Add(8*time.Minute),
	); err == nil {
		t.Fatal("startFragment() on paused job error = nil")
	}
	resumed, err := store.resumeJob(context.Background(), "job-a", now.Add(9*time.Minute))
	if err != nil || resumed.Status != JobStatusQueued {
		t.Fatalf("resumeJob() = (%+v, %v)", resumed, err)
	}
	claimedAgain, ok, err := store.claimGenerationTask(
		context.Background(), now.Add(10*time.Minute),
	)
	if err != nil || !ok || claimedAgain.JobID != "job-a" {
		t.Fatalf("claim resumed job = (%+v, %v, %v)", claimedAgain, ok, err)
	}
	canceled, err := store.cancelJob(context.Background(), "job-a", now.Add(11*time.Minute))
	if err != nil || canceled.Status != JobStatusCanceled {
		t.Fatalf("cancelJob() = (%+v, %v)", canceled, err)
	}
	next, ok, err := store.claimGenerationTask(context.Background(), now.Add(12*time.Minute))
	if err != nil || !ok || next.JobID != "job-b" {
		t.Fatalf("claim next book = (%+v, %v, %v), want job-b", next, ok, err)
	}
}

func TestServerCloseRequeuesActiveFragmentAndRestartCompletes(t *testing.T) {
	store, jobID, fragmentID := newQueueRunnerStore(t)
	blocking := newBlockingQueueTTS()
	server := newQueueTestServer(t, store, blocking)

	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		server.Close()
		t.Fatal("first server did not start TTS")
	}
	server.Close()

	jobResource, ok, err := store.job(context.Background(), jobID)
	if err != nil || !ok || jobResource.Status != JobStatusQueued {
		t.Fatalf("job after Server.Close() = (%+v, %v, %v), want queued", jobResource, ok, err)
	}
	fragment := store.fragments[fragmentID].Resource
	if fragment.Status != FragmentStatusPending || fragment.Error != "" {
		t.Fatalf("fragment after Server.Close() = %+v, want clean pending", fragment)
	}

	restarted := newQueueTestServer(t, store, successfulQueueTTS{})
	defer restarted.Close()
	waitForMemoryJobStatus(t, store, jobID, JobStatusCompleted)
	if got := store.fragments[fragmentID].Resource.Attempt; got != 2 {
		t.Fatalf("fragment attempts after restart = %d, want 2", got)
	}
}

func TestJobControlEndpointsPauseResumeAndQueueView(t *testing.T) {
	store, jobID, fragmentID := newQueueRunnerStore(t)
	tts := newPauseResumeQueueTTS()
	server := newQueueTestServer(t, store, tts)
	defer server.Close()

	select {
	case <-tts.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start first TTS attempt")
	}

	pause := queueEndpointRequest(
		t, server.Handler(), http.MethodPost, "/v1/job/"+jobID+"/pause",
	)
	if pause.Code != http.StatusOK {
		t.Fatalf("pause status = %d, body = %s", pause.Code, pause.Body)
	}
	var paused JobResource
	if err := json.NewDecoder(pause.Body).Decode(&paused); err != nil {
		t.Fatalf("decode paused job: %v", err)
	}
	if paused.Status != JobStatusPaused {
		t.Fatalf("paused job = %+v", paused)
	}
	waitForQueueRunnerIdle(t, server.runner)
	persistedPaused, ok, err := store.job(context.Background(), jobID)
	if err != nil || !ok || persistedPaused.Status != JobStatusPaused {
		t.Fatalf(
			"job after pause interrupt release = (%+v, %v, %v), want paused",
			persistedPaused,
			ok,
			err,
		)
	}

	queue := queueEndpointRequest(t, server.Handler(), http.MethodGet, "/v1/queue")
	if queue.Code != http.StatusOK {
		t.Fatalf("queue status = %d, body = %s", queue.Code, queue.Body)
	}
	var snapshot GenerationQueueResponse
	if err := json.NewDecoder(queue.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	if snapshot.TotalJobs != 1 || snapshot.TotalFragments != 1 ||
		snapshot.Jobs[0].Job.Status != JobStatusPaused ||
		snapshot.Fragments[0].Fragment.ID != fragmentID ||
		snapshot.Fragments[0].Fragment.Status != FragmentStatusPending {
		t.Fatalf("paused queue snapshot = %+v", snapshot)
	}

	resume := queueEndpointRequest(
		t, server.Handler(), http.MethodPost, "/v1/job/"+jobID+"/resume",
	)
	if resume.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s", resume.Code, resume.Body)
	}
	waitForMemoryJobStatus(t, store, jobID, JobStatusCompleted)

	cancel := queueEndpointRequest(
		t, server.Handler(), http.MethodPost, "/v1/job/"+jobID+"/cancel",
	)
	if cancel.Code != http.StatusConflict {
		t.Fatalf("cancel completed job status = %d, body = %s", cancel.Code, cancel.Body)
	}
}

func TestCancelActiveJobRemainsCanceledAfterInterruptRelease(t *testing.T) {
	store, jobID, fragmentID := newQueueRunnerStore(t)
	tts := newBlockingQueueTTS()
	server := newQueueTestServer(t, store, tts)
	defer server.Close()

	select {
	case <-tts.started:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start TTS before cancel")
	}
	response := queueEndpointRequest(
		t, server.Handler(), http.MethodPost, "/v1/job/"+jobID+"/cancel",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", response.Code, response.Body)
	}
	waitForQueueRunnerIdle(t, server.runner)

	resource, ok, err := store.job(context.Background(), jobID)
	if err != nil || !ok || resource.Status != JobStatusCanceled {
		t.Fatalf(
			"job after cancel interrupt release = (%+v, %v, %v), want canceled",
			resource,
			ok,
			err,
		)
	}
	fragment := store.fragments[fragmentID].Resource
	if fragment.Status != FragmentStatusPending || fragment.Error != "" {
		t.Fatalf("fragment after cancel interrupt release = %+v", fragment)
	}
	snapshot, err := store.generationQueue(context.Background())
	if err != nil || len(snapshot.Jobs) != 0 || len(snapshot.Fragments) != 0 {
		t.Fatalf("queue after cancel = (%+v, %v), want empty", snapshot, err)
	}
}

func TestEditCompletedFragmentDuringRunningTaskIsDurablyReclaimed(t *testing.T) {
	store := newMemoryStore()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	parsed := book.Book{Title: "Live edit book", Chapters: []book.Chapter{{
		Number: 1,
		Title:  "Live edit chapter",
		Segments: []string{
			"Already generated fragment.",
			"Currently generating fragment.",
		},
	}}}
	if _, err := store.createBook(context.Background(), "live-edit-book", parsed, now); err != nil {
		t.Fatalf("create live edit book: %v", err)
	}
	if _, err := store.createVoice(
		context.Background(), "live-edit-voice", "Voice", "wav", "audio/wav",
		"Reference.", []byte("voice"), now,
	); err != nil {
		t.Fatalf("create live edit voice: %v", err)
	}
	if _, _, err := store.createJob(
		context.Background(), "live-edit-job", "live-edit-book", "live-edit-voice",
		[]string{"live-edit-fragment-1", "live-edit-fragment-2"},
		[]string{"live-edit-revision-1", "live-edit-revision-2"},
		defaultGenerationSettings(), now,
	); err != nil {
		t.Fatalf("create live edit job: %v", err)
	}

	tts := newEditWhileRunningQueueTTS()
	server := newQueueTestServer(t, store, tts)
	defer server.Close()
	select {
	case <-tts.secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not reach the second fragment")
	}

	request := httptest.NewRequest(
		http.MethodPatch,
		"/v1/fragment/live-edit-fragment-1",
		strings.NewReader(`{"new_text":"Edited while the other fragment runs."}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("edit running job fragment status = %d, body = %s", response.Code, response.Body)
	}
	close(tts.releaseSecond)
	waitForMemoryJobStatus(t, store, "live-edit-job", JobStatusCompleted)

	first := store.fragments["live-edit-fragment-1"].Resource
	second := store.fragments["live-edit-fragment-2"].Resource
	if first.Status != FragmentStatusReady || first.Attempt != 2 ||
		first.Text != "Edited while the other fragment runs." {
		t.Fatalf("edited fragment after durable reclaim = %+v", first)
	}
	if second.Status != FragmentStatusReady || second.Attempt != 1 {
		t.Fatalf("second fragment after durable reclaim = %+v", second)
	}
	if got := tts.calls.Load(); got != 3 {
		t.Fatalf("TTS calls = %d, want initial two plus reclaimed edit", got)
	}
}

func TestMemoryQueuedJobRemainsClaimableAcrossRepeatedEdits(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	now := time.Date(2026, time.August, 2, 15, 0, 0, 0, time.UTC)
	parsed := book.Book{Title: "Queued edits", Chapters: []book.Chapter{{
		Number: 1,
		Title:  "Chapter",
		Segments: []string{
			"First warning.",
			"Second warning.",
		},
	}}}
	if _, err := store.createBook(context.Background(), "queued-edits-book", parsed, now); err != nil {
		t.Fatalf("create queued edits book: %v", err)
	}
	if _, err := store.createVoice(
		context.Background(), "queued-edits-voice", "Voice", "wav", "audio/wav",
		"Reference.", []byte("voice"), now,
	); err != nil {
		t.Fatalf("create queued edits voice: %v", err)
	}
	fragmentIDs := []string{"queued-edit-1", "queued-edit-2"}
	if _, _, err := store.createJob(
		context.Background(), "queued-edits-job", "queued-edits-book",
		"queued-edits-voice", fragmentIDs,
		[]string{"queued-edit-revision-1", "queued-edit-revision-2"},
		defaultGenerationSettings(), now,
	); err != nil {
		t.Fatalf("create queued edits job: %v", err)
	}

	store.mu.Lock()
	for _, fragmentID := range fragmentIDs {
		fragment := store.fragments[fragmentID]
		fragment.Resource.Status = FragmentStatusWarning
		fragment.Resource.WarningCode = "transcript_mismatch"
	}
	recomputeJob(store.jobs["queued-edits-job"], store.fragments, now.Add(time.Minute))
	store.mu.Unlock()

	for index, fragmentID := range fragmentIDs {
		if _, _, err := store.editAndPrepareFragment(
			context.Background(), fragmentID,
			fmt.Sprintf("Edited warning %d.", index+1),
			fmt.Sprintf("queued-edit-revision-%d", index+3),
			now.Add(time.Duration(index+2)*time.Minute),
		); err != nil {
			t.Fatalf("edit queued warning %d: %v", index+1, err)
		}
		resource, ok, err := store.job(context.Background(), "queued-edits-job")
		if err != nil || !ok || resource.Status != JobStatusQueued {
			t.Fatalf(
				"job after queued edit %d = (%+v, %v, %v), want queued",
				index+1, resource, ok, err,
			)
		}
	}

	task, ok, err := store.claimGenerationTask(
		context.Background(), now.Add(5*time.Minute),
	)
	if err != nil || !ok || task.JobID != "queued-edits-job" ||
		len(task.FragmentIDs) != len(fragmentIDs) {
		t.Fatalf("claim repeatedly edited job = (%+v, %v, %v)", task, ok, err)
	}
}

func TestMemoryFinishAndRecoveryNormalizeOrphanedGeneratingFragment(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	now := time.Date(2026, time.August, 2, 16, 0, 0, 0, time.UTC)
	store.jobs["orphan-job"] = queueTestJob(
		"orphan-job", "orphan-book", JobStatusQueued, now, "orphan-fragment",
	)
	store.fragments["orphan-fragment"] = queueTestFragment(
		"orphan-fragment", "orphan-job", FragmentStatusPending, 1,
	)

	task, ok, err := store.claimGenerationTask(context.Background(), now.Add(time.Minute))
	if err != nil || !ok || task.JobID != "orphan-job" {
		t.Fatalf("claim orphan fixture = (%+v, %v, %v)", task, ok, err)
	}
	if _, err := store.startFragment(
		context.Background(), "orphan-fragment", now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("start orphan fixture fragment: %v", err)
	}
	if err := store.finishTask(
		context.Background(), "orphan-job", now.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("finish task with orphaned generating fragment: %v", err)
	}
	assertMemoryQueueFragment(t, store, "orphan-job", "orphan-fragment")

	// Also defend against the poison state written by older builds.
	store.mu.Lock()
	store.jobs["orphan-job"].Resource.Status = JobStatusQueued
	store.fragments["orphan-fragment"].Resource.Status = FragmentStatusGenerating
	store.mu.Unlock()
	if err := store.recoverGenerationQueue(
		context.Background(), now.Add(4*time.Minute),
	); err != nil {
		t.Fatalf("recover queued+generating poison state: %v", err)
	}
	assertMemoryQueueFragment(t, store, "orphan-job", "orphan-fragment")
}

func assertMemoryQueueFragment(
	t *testing.T,
	store *memoryStore,
	jobID, fragmentID string,
) {
	t.Helper()
	resource, ok, err := store.job(context.Background(), jobID)
	if err != nil || !ok || resource.Status != JobStatusQueued {
		t.Fatalf("queue job = (%+v, %v, %v), want queued", resource, ok, err)
	}
	store.mu.RLock()
	fragment := store.fragments[fragmentID].Resource
	store.mu.RUnlock()
	if fragment.Status != FragmentStatusPending || fragment.Error != "" {
		t.Fatalf("queue fragment = %+v, want clean pending", fragment)
	}
	task, claimed, err := store.claimGenerationTask(context.Background(), resource.UpdatedAt)
	if err != nil || !claimed || task.JobID != jobID {
		t.Fatalf("reclaim normalized task = (%+v, %v, %v)", task, claimed, err)
	}
	// Restore the fixture so the helper can be used for another normalization path.
	store.mu.Lock()
	store.jobs[jobID].Resource.Status = JobStatusQueued
	store.mu.Unlock()
}

func TestPostgresGenerationQueueRecoveryControlsAndFIFO(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := preparePostgresStore(t, ctx, databaseURL)
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	parsed := book.Book{Title: "Queue book A", Chapters: []book.Chapter{{
		Number: 1, Title: "Chapter A", Segments: []string{"Fragment A."},
	}}}
	if _, err := store.createBook(ctx, "queue-book-a", parsed, now); err != nil {
		t.Fatalf("create first queue book: %v", err)
	}
	parsed.Title = "Queue book B"
	parsed.Chapters[0].Title = "Chapter B"
	parsed.Chapters[0].Segments[0] = "Fragment B."
	if _, err := store.createBook(ctx, "queue-book-b", parsed, now); err != nil {
		t.Fatalf("create second queue book: %v", err)
	}
	if _, err := store.createVoice(
		ctx, "queue-voice", "Voice", "wav", "audio/wav", "Reference.", []byte("voice"), now,
	); err != nil {
		t.Fatalf("create queue voice: %v", err)
	}
	settings := defaultGenerationSettings()
	if _, _, err := store.createJob(
		ctx, "queue-job-a", "queue-book-a", "queue-voice",
		[]string{"queue-fragment-a"}, []string{"queue-revision-a"}, settings, now,
	); err != nil {
		t.Fatalf("create first queue job: %v", err)
	}
	if _, _, err := store.createJob(
		ctx, "queue-job-b", "queue-book-b", "queue-voice",
		[]string{"queue-fragment-b"}, []string{"queue-revision-b"}, settings,
		now.Add(time.Minute),
	); err != nil {
		t.Fatalf("create second queue job: %v", err)
	}

	task, ok, err := store.claimGenerationTask(ctx, now.Add(2*time.Minute))
	if err != nil || !ok || task.JobID != "queue-job-a" {
		t.Fatalf("claim first PostgreSQL task = (%+v, %v, %v)", task, ok, err)
	}
	if _, err := store.startFragment(ctx, "queue-fragment-a", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("start first PostgreSQL fragment: %v", err)
	}
	if err := store.failFragment(
		ctx, "queue-fragment-a", "generation interrupted", now.Add(4*time.Minute),
	); err != nil {
		t.Fatalf("persist legacy interruption: %v", err)
	}
	if err := store.recoverGenerationQueue(ctx, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("recover PostgreSQL queue: %v", err)
	}
	assertPostgresJobState(
		t, ctx, store, "queue-job-a", JobStatusQueued, 1, 0, 0, 0,
	)

	task, ok, err = store.claimGenerationTask(ctx, now.Add(6*time.Minute))
	if err != nil || !ok || task.JobID != "queue-job-a" {
		t.Fatalf("reclaim recovered PostgreSQL task = (%+v, %v, %v)", task, ok, err)
	}
	if _, err := store.startFragment(ctx, "queue-fragment-a", now.Add(7*time.Minute)); err != nil {
		t.Fatalf("restart first PostgreSQL fragment: %v", err)
	}
	paused, err := store.pauseJob(ctx, "queue-job-a", now.Add(8*time.Minute))
	if err != nil || paused.Status != JobStatusPaused {
		t.Fatalf("pause PostgreSQL job = (%+v, %v)", paused, err)
	}
	if err := store.releaseGenerationTask(
		ctx, "queue-job-a", "queue-fragment-a", now.Add(9*time.Minute),
	); err != nil {
		t.Fatalf("release paused PostgreSQL task: %v", err)
	}
	assertPostgresJobState(
		t, ctx, store, "queue-job-a", JobStatusPaused, 1, 0, 0, 0,
	)
	resumed, err := store.resumeJob(ctx, "queue-job-a", now.Add(10*time.Minute))
	if err != nil || resumed.Status != JobStatusQueued {
		t.Fatalf("resume PostgreSQL job = (%+v, %v)", resumed, err)
	}
	task, ok, err = store.claimGenerationTask(ctx, now.Add(11*time.Minute))
	if err != nil || !ok || task.JobID != "queue-job-a" {
		t.Fatalf("claim resumed PostgreSQL job = (%+v, %v, %v)", task, ok, err)
	}
	if _, err := store.startFragment(ctx, "queue-fragment-a", now.Add(12*time.Minute)); err != nil {
		t.Fatalf("start resumed PostgreSQL fragment: %v", err)
	}
	canceled, err := store.cancelJob(ctx, "queue-job-a", now.Add(13*time.Minute))
	if err != nil || canceled.Status != JobStatusCanceled {
		t.Fatalf("cancel PostgreSQL job = (%+v, %v)", canceled, err)
	}
	if err := store.releaseGenerationTask(
		ctx, "queue-job-a", "queue-fragment-a", now.Add(14*time.Minute),
	); err != nil {
		t.Fatalf("release canceled PostgreSQL task: %v", err)
	}
	assertPostgresJobState(
		t, ctx, store, "queue-job-a", JobStatusCanceled, 1, 0, 0, 0,
	)

	next, ok, err := store.claimGenerationTask(ctx, now.Add(15*time.Minute))
	if err != nil || !ok || next.JobID != "queue-job-b" {
		t.Fatalf("claim second PostgreSQL book = (%+v, %v, %v)", next, ok, err)
	}
	snapshot, err := store.generationQueue(ctx)
	if err != nil {
		t.Fatalf("PostgreSQL generationQueue() error = %v", err)
	}
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Job.ID != "queue-job-b" ||
		len(snapshot.Fragments) != 1 || snapshot.Fragments[0].BookTitle != "Queue book B" {
		t.Fatalf("PostgreSQL queue snapshot = %+v", snapshot)
	}
}

func TestPostgresGenerationQueueNormalizesQueuedMutationsAndOrphans(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := preparePostgresStore(t, ctx, databaseURL)
	now := time.Date(2026, time.August, 2, 17, 0, 0, 0, time.UTC)
	parsed := book.Book{Title: "PostgreSQL queue invariants", Chapters: []book.Chapter{{
		Number:   1,
		Title:    "Chapter",
		Segments: []string{"First warning.", "Second warning."},
	}}}
	if _, err := store.createBook(ctx, "pg-invariant-book", parsed, now); err != nil {
		t.Fatalf("create invariant book: %v", err)
	}
	if _, err := store.createVoice(
		ctx, "pg-invariant-voice", "Voice", "wav", "audio/wav",
		"Reference.", []byte("voice"), now,
	); err != nil {
		t.Fatalf("create invariant voice: %v", err)
	}
	fragmentIDs := []string{"pg-invariant-fragment-1", "pg-invariant-fragment-2"}
	if _, _, err := store.createJob(
		ctx, "pg-invariant-job", "pg-invariant-book", "pg-invariant-voice",
		fragmentIDs,
		[]string{"pg-invariant-revision-1", "pg-invariant-revision-2"},
		defaultGenerationSettings(), now,
	); err != nil {
		t.Fatalf("create invariant job: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE job_fragments
		SET status = 'warning', warning_code = 'transcript_mismatch'
		WHERE job_id = 'pg-invariant-job'`); err != nil {
		t.Fatalf("prepare invariant warning fragments: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE jobs
		SET status = 'completed_with_warnings', fragments_pending = 0,
			fragments_warnings = 2
		WHERE id = 'pg-invariant-job'`); err != nil {
		t.Fatalf("prepare invariant warning job: %v", err)
	}

	for index, fragmentID := range fragmentIDs {
		if _, _, err := store.editAndPrepareFragment(
			ctx, fragmentID, fmt.Sprintf("Edited PostgreSQL warning %d.", index+1),
			fmt.Sprintf("pg-invariant-revision-%d", index+3),
			now.Add(time.Duration(index+1)*time.Minute),
		); err != nil {
			t.Fatalf("edit PostgreSQL queued warning %d: %v", index+1, err)
		}
		assertPostgresJobState(
			t,
			ctx,
			store,
			"pg-invariant-job",
			JobStatusQueued,
			index+1,
			0,
			len(fragmentIDs)-index-1,
			0,
		)
	}

	task, ok, err := store.claimGenerationTask(ctx, now.Add(3*time.Minute))
	if err != nil || !ok || task.JobID != "pg-invariant-job" {
		t.Fatalf("claim edited PostgreSQL job = (%+v, %v, %v)", task, ok, err)
	}
	if _, err := store.startFragment(
		ctx, fragmentIDs[0], now.Add(4*time.Minute),
	); err != nil {
		t.Fatalf("start PostgreSQL orphan fixture: %v", err)
	}
	if err := store.finishTask(ctx, "pg-invariant-job", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("finish PostgreSQL orphan fixture: %v", err)
	}
	assertPostgresJobState(
		t, ctx, store, "pg-invariant-job", JobStatusQueued, 2, 0, 0, 0,
	)
	assertPostgresFragmentStatus(
		t, ctx, store, fragmentIDs[0], FragmentStatusPending,
	)

	if _, err := store.pool.Exec(ctx, `UPDATE job_fragments
		SET status = 'generating'
		WHERE id = $1`, fragmentIDs[0]); err != nil {
		t.Fatalf("write legacy queued+generating poison state: %v", err)
	}
	if err := store.recoverGenerationQueue(ctx, now.Add(6*time.Minute)); err != nil {
		t.Fatalf("recover PostgreSQL queued+generating poison state: %v", err)
	}
	assertPostgresJobState(
		t, ctx, store, "pg-invariant-job", JobStatusQueued, 2, 0, 0, 0,
	)
	assertPostgresFragmentStatus(
		t, ctx, store, fragmentIDs[0], FragmentStatusPending,
	)
	if task, ok, err = store.claimGenerationTask(ctx, now.Add(7*time.Minute)); err != nil || !ok || task.JobID != "pg-invariant-job" {
		t.Fatalf("reclaim normalized PostgreSQL job = (%+v, %v, %v)", task, ok, err)
	}
}

func assertPostgresFragmentStatus(
	t *testing.T,
	ctx context.Context,
	store *PostgresStore,
	fragmentID string,
	want FragmentStatus,
) {
	t.Helper()
	var status FragmentStatus
	var message string
	if err := store.pool.QueryRow(ctx, `SELECT status, error_message
		FROM job_fragments WHERE id = $1`, fragmentID).Scan(&status, &message); err != nil {
		t.Fatalf("query PostgreSQL fragment %q: %v", fragmentID, err)
	}
	if status != want || message != "" {
		t.Fatalf(
			"PostgreSQL fragment %q = (%q, %q), want (%q, empty)",
			fragmentID, status, message, want,
		)
	}
}

func queueTestJob(
	id, bookID string,
	status JobStatus,
	createdAt time.Time,
	fragmentIDs ...string,
) *jobRecord {
	return &jobRecord{
		Resource: JobResource{
			ID:               id,
			BookID:           bookID,
			VoiceID:          "voice-1",
			Status:           status,
			FragmentsCount:   len(fragmentIDs),
			FragmentsPending: len(fragmentIDs),
			CreatedAt:        createdAt,
			UpdatedAt:        createdAt,
		},
		FragmentIDs: fragmentIDs,
	}
}

func queueTestFragment(
	id, jobID string,
	status FragmentStatus,
	ordinal int,
) *fragmentRecord {
	return &fragmentRecord{
		Resource: FragmentResource{
			ID:        id,
			JobID:     jobID,
			Text:      "Queue fragment.",
			Status:    status,
			Ordinal:   ordinal,
			UpdatedAt: time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC),
		},
		ChapterTitle: "Queue chapter",
	}
}

func newQueueRunnerStore(t *testing.T) (*memoryStore, string, string) {
	t.Helper()
	store := newMemoryStore()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	parsed := book.Book{Title: "Restart book", Chapters: []book.Chapter{{
		Number: 1, Title: "Restart chapter", Segments: []string{"Restart fragment."},
	}}}
	if _, err := store.createBook(context.Background(), "restart-book", parsed, now); err != nil {
		t.Fatalf("create restart book: %v", err)
	}
	if _, err := store.createVoice(
		context.Background(), "restart-voice", "Voice", "wav", "audio/wav",
		"Reference.", []byte("voice"), now,
	); err != nil {
		t.Fatalf("create restart voice: %v", err)
	}
	if _, _, err := store.createJob(
		context.Background(), "restart-job", "restart-book", "restart-voice",
		[]string{"restart-fragment"}, []string{"restart-revision"},
		defaultGenerationSettings(), now,
	); err != nil {
		t.Fatalf("create restart job: %v", err)
	}
	return store, "restart-job", "restart-fragment"
}

func newQueueTestServer(t *testing.T, store repository, tts TTSClient) *Server {
	t.Helper()
	var ids atomic.Int64
	server, err := newServerWithRepository(Dependencies{
		TTS: tts,
		STT: successfulQueueSTT{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
		ID: func() string {
			return fmt.Sprintf("queue-request-%d", ids.Add(1))
		},
		WorkerTimeout: time.Second,
		QueueSize:     1,
	}, store)
	if err != nil {
		t.Fatalf("newServerWithRepository() error = %v", err)
	}
	return server
}

type blockingQueueTTS struct {
	started chan struct{}
	once    sync.Once
}

func newBlockingQueueTTS() *blockingQueueTTS {
	return &blockingQueueTTS{started: make(chan struct{})}
}

func (c *blockingQueueTTS) Generate(
	ctx context.Context,
	_ TTSRequest,
) (TTSResult, error) {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	return TTSResult{}, ctx.Err()
}

type pauseResumeQueueTTS struct {
	firstStarted chan struct{}
	once         sync.Once
	calls        atomic.Int64
}

func newPauseResumeQueueTTS() *pauseResumeQueueTTS {
	return &pauseResumeQueueTTS{firstStarted: make(chan struct{})}
}

func (c *pauseResumeQueueTTS) Generate(
	ctx context.Context,
	request TTSRequest,
) (TTSResult, error) {
	if c.calls.Add(1) == 1 {
		c.once.Do(func() { close(c.firstStarted) })
		<-ctx.Done()
		return TTSResult{}, ctx.Err()
	}
	return successfulQueueTTS{}.Generate(ctx, request)
}

type successfulQueueTTS struct{}

func (successfulQueueTTS) Generate(
	ctx context.Context,
	request TTSRequest,
) (TTSResult, error) {
	if err := ctx.Err(); err != nil {
		return TTSResult{}, err
	}
	return TTSResult{
		RequestID:   request.RequestID,
		AudioPCM:    []byte{0, 0, 1, 0},
		SampleRate:  24_000,
		Channels:    1,
		SampleWidth: 2,
		DurationMS:  1,
	}, nil
}

type editWhileRunningQueueTTS struct {
	calls         atomic.Int64
	secondStarted chan struct{}
	releaseSecond chan struct{}
	once          sync.Once
}

func newEditWhileRunningQueueTTS() *editWhileRunningQueueTTS {
	return &editWhileRunningQueueTTS{
		secondStarted: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
}

func (c *editWhileRunningQueueTTS) Generate(
	ctx context.Context,
	request TTSRequest,
) (TTSResult, error) {
	if c.calls.Add(1) == 2 {
		c.once.Do(func() { close(c.secondStarted) })
		select {
		case <-ctx.Done():
			return TTSResult{}, ctx.Err()
		case <-c.releaseSecond:
		}
	}
	return successfulQueueTTS{}.Generate(ctx, request)
}

type successfulQueueSTT struct{}

func (successfulQueueSTT) Transcribe(
	ctx context.Context,
	request STTRequest,
) (STTResult, error) {
	if err := ctx.Err(); err != nil {
		return STTResult{}, err
	}
	return STTResult{
		RequestID:  request.RequestID,
		Text:       request.ExpectedText,
		Language:   "ru",
		DurationMS: 1,
	}, nil
}

func waitForMemoryJobStatus(
	t *testing.T,
	store *memoryStore,
	jobID string,
	want JobStatus,
) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		resource, ok, err := store.job(context.Background(), jobID)
		if err != nil || !ok {
			t.Fatalf("job(%q) = (%+v, %v, %v)", jobID, resource, ok, err)
		}
		if resource.Status == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("job %q did not reach %q; last = %+v", jobID, want, resource)
		case <-ticker.C:
		}
	}
}

func waitForQueueRunnerIdle(t *testing.T, runner *jobRunner) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		runner.mu.Lock()
		active := runner.activeJobID
		runner.mu.Unlock()
		if active == "" {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("generation runner still active for job %q", active)
		case <-ticker.C:
		}
	}
}

func queueEndpointRequest(
	t *testing.T,
	handler http.Handler,
	method, target string,
) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, target, nil))
	return response
}
