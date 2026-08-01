package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreUserDeletionRulesAndCleanup(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	ctx := context.Background()
	created := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	store.books["book-1"] = &bookRecord{
		Resource:    BookResource{ID: "book-1"},
		LatestJobID: "job-new",
	}
	store.voicesMap["voice-1"] = &voiceRecord{
		Payload: voicePayload{Resource: VoiceResource{ID: "voice-1"}},
	}
	store.jobs["job-old"] = &jobRecord{Resource: JobResource{
		ID:        "job-old",
		BookID:    "book-1",
		VoiceID:   "voice-1",
		Status:    JobStatusCompleted,
		CreatedAt: created,
	}}
	store.jobs["job-new"] = &jobRecord{
		Resource: JobResource{
			ID:        "job-new",
			BookID:    "book-1",
			VoiceID:   "voice-1",
			Status:    JobStatusRunning,
			CreatedAt: created.Add(time.Minute),
		},
		FragmentIDs: []string{"fragment-1"},
	}
	store.fragments["fragment-1"] = &fragmentRecord{
		Resource: FragmentResource{ID: "fragment-1", JobID: "job-new"},
	}

	if deleted, err := store.deleteJobForUser(ctx, "job-new"); deleted || !errors.Is(err, errConflict) {
		t.Fatalf("delete active job = (%v, %v), want conflict", deleted, err)
	}
	store.jobs["job-new"].Resource.Status = JobStatusCompletedWithWarnings
	store.rewriteTasks["rewrite-1"] = &rewriteTaskRecord{
		Resource: RewriteTaskResource{
			ID:     "rewrite-1",
			JobID:  "job-new",
			Status: RewriteTaskStatusRunning,
		},
	}
	if deleted, err := store.deleteJobForUser(ctx, "job-new"); deleted || !errors.Is(err, errConflict) {
		t.Fatalf("delete job with active rewrite = (%v, %v), want conflict", deleted, err)
	}
	store.rewriteTasks["rewrite-1"].Resource.Status = RewriteTaskStatusCompleted

	deleted, err := store.deleteJobForUser(ctx, "job-new")
	if err != nil || !deleted {
		t.Fatalf("delete terminal job = (%v, %v), want true", deleted, err)
	}
	if store.jobs["job-new"] != nil || store.fragments["fragment-1"] != nil ||
		store.rewriteTasks["rewrite-1"] != nil {
		t.Fatalf("job-related memory state was not fully deleted")
	}
	if got := store.books["book-1"].LatestJobID; got != "job-old" {
		t.Fatalf("latest job after deletion = %q, want job-old", got)
	}

	if deleted, err := store.deleteVoice(ctx, "voice-1"); deleted || !errors.Is(err, errConflict) {
		t.Fatalf("delete referenced voice = (%v, %v), want conflict", deleted, err)
	}
	if deleted, err := store.deleteJobForUser(ctx, "job-old"); err != nil || !deleted {
		t.Fatalf("delete old job = (%v, %v), want true", deleted, err)
	}
	if deleted, err := store.deleteVoice(ctx, "voice-1"); err != nil || !deleted {
		t.Fatalf("delete unreferenced voice = (%v, %v), want true", deleted, err)
	}
	if _, exists := store.voicesMap["voice-1"]; exists {
		t.Fatal("voice remains in memory store")
	}
	if deleted, err := store.deleteVoice(ctx, "missing"); err != nil || deleted {
		t.Fatalf("delete missing voice = (%v, %v), want false nil", deleted, err)
	}
}
