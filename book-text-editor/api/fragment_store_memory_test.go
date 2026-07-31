package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryFragmentWorkflowCatalogAndEdit(t *testing.T) {
	t.Parallel()

	store, now := fragmentWorkflowMemoryFixture()
	snapshot, found, err := store.jobFragments(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("jobFragments() error = %v", err)
	}
	if !found || snapshot.Job.ID != "job-1" || len(snapshot.Fragments) != 2 {
		t.Fatalf("jobFragments() = %#v, found=%t", snapshot, found)
	}
	if !snapshot.Fragments[0].AudioAvailable || !snapshot.Fragments[0].Editable {
		t.Fatalf("ready fragment view = %#v", snapshot.Fragments[0])
	}
	if snapshot.Fragments[1].AudioAvailable || snapshot.Fragments[1].Editable {
		t.Fatalf("pending fragment view = %#v", snapshot.Fragments[1])
	}

	updated, task, err := store.editAndPrepareFragment(
		context.Background(),
		"fragment-1",
		"Исправленный текст.",
		"revision-2",
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("editAndPrepareFragment() error = %v", err)
	}
	if updated.Status != FragmentStatusPending ||
		updated.WarningCode != "text_edited" ||
		updated.Text != "Исправленный текст." {
		t.Fatalf("updated fragment = %+v", updated)
	}
	if task.JobID != "job-1" || len(task.FragmentIDs) != 1 ||
		task.FragmentIDs[0] != "fragment-1" || !task.IsRetry {
		t.Fatalf("prepared task = %+v", task)
	}
	stored := store.fragments["fragment-1"]
	if len(stored.AudioPCM) != 0 || stored.DurationMS != 0 ||
		stored.SampleRate != 0 || stored.Channels != 0 || stored.SampleWidth != 0 {
		t.Fatalf("edited fragment retained stale audio: %+v", stored)
	}
	if len(stored.TextRevisions) != 2 ||
		stored.TextRevisions[1].ID != "revision-2" {
		t.Fatalf("text revisions = %#v", stored.TextRevisions)
	}
	if store.jobs["job-1"].Resource.Status != JobStatusRunning {
		t.Fatalf("active job status = %q, want running", store.jobs["job-1"].Resource.Status)
	}
}

func TestMemoryFragmentWorkflowBlocksActiveRewrite(t *testing.T) {
	t.Parallel()

	store, now := fragmentWorkflowMemoryFixture()
	store.rewriteTasks["rewrite-1"] = &rewriteTaskRecord{
		Resource: RewriteTaskResource{
			ID:     "rewrite-1",
			JobID:  "job-1",
			Status: RewriteTaskStatusRunning,
		},
	}

	_, _, err := store.editAndPrepareFragment(
		context.Background(),
		"fragment-1",
		"Исправленный текст.",
		"revision-2",
		now,
	)
	if !errors.Is(err, errConflict) {
		t.Fatalf("editAndPrepareFragment() error = %v, want errConflict", err)
	}
	if store.fragments["fragment-1"].Resource.Text != "Исходный текст." {
		t.Fatal("blocked edit changed the fragment text")
	}
}

func TestMemoryDownloadableChapterRequiresEveryFragmentReady(t *testing.T) {
	t.Parallel()

	store, _ := fragmentWorkflowMemoryFixture()
	_, err := store.downloadableChapterSnapshot(context.Background(), "job-1", 1)
	if !errors.Is(err, errConflict) {
		t.Fatalf("downloadableChapterSnapshot() error = %v, want errConflict", err)
	}

	pending := store.fragments["fragment-2"]
	pending.Resource.Status = FragmentStatusReady
	pending.AudioPCM = []byte{0, 0, 1, 0}
	pending.SampleRate = 24_000
	pending.Channels = 1
	pending.SampleWidth = 2
	pending.DurationMS = 1

	snapshot, err := store.downloadableChapterSnapshot(
		context.Background(),
		"job-1",
		1,
	)
	if err != nil {
		t.Fatalf("downloadableChapterSnapshot() error = %v", err)
	}
	if len(snapshot.Fragments) != 2 {
		t.Fatalf("chapter fragment count = %d, want 2", len(snapshot.Fragments))
	}
	// The snapshot must own its bytes rather than expose storage memory.
	snapshot.Fragments[0].AudioPCM[0] = 99
	if store.fragments["fragment-1"].AudioPCM[0] == 99 {
		t.Fatal("chapter snapshot aliases persistent PCM")
	}
}

func fragmentWorkflowMemoryFixture() (*memoryStore, time.Time) {
	now := time.Date(2026, time.July, 31, 9, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	store.books["book-1"] = &bookRecord{Resource: BookResource{
		ID:             "book-1",
		Title:          "Книга",
		ChaptersCount:  1,
		FragmentsCount: 2,
	}}
	store.voicesMap["voice-1"] = &voiceRecord{Payload: voicePayload{
		Resource: VoiceResource{ID: "voice-1", Name: "Голос"},
	}}
	store.jobs["job-1"] = &jobRecord{
		Resource: JobResource{
			ID:               "job-1",
			BookID:           "book-1",
			VoiceID:          "voice-1",
			Status:           JobStatusRunning,
			FragmentsCount:   2,
			FragmentsPending: 1,
			FragmentsReady:   1,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		FragmentIDs: []string{"fragment-1", "fragment-2"},
	}
	original := newFragmentTextRevision(
		"revision-1",
		"fragment-1",
		1,
		FragmentTextRevisionSourceOriginal,
		"Исходный текст.",
		"",
		"",
		now,
	)
	store.fragments["fragment-1"] = &fragmentRecord{
		Resource: FragmentResource{
			ID:            "fragment-1",
			JobID:         "job-1",
			ChapterNumber: 1,
			Ordinal:       1,
			Text:          "Исходный текст.",
			STTText:       "Исходный текст.",
			Status:        FragmentStatusReady,
			Attempt:       1,
			UpdatedAt:     now,
		},
		ChapterTitle: "Глава",
		AudioPCM:     []byte{0, 0, 1, 0},
		SampleRate:   24_000,
		Channels:     1,
		SampleWidth:  2,
		DurationMS:   1,
		TextRevisions: []FragmentTextRevisionResource{
			original,
		},
	}
	store.fragments["fragment-2"] = &fragmentRecord{
		Resource: FragmentResource{
			ID:            "fragment-2",
			JobID:         "job-1",
			ChapterNumber: 1,
			Ordinal:       2,
			Text:          "Следующий текст.",
			Status:        FragmentStatusPending,
			UpdatedAt:     now,
		},
		ChapterTitle: "Глава",
	}
	store.textRevisionIDs[original.ID] = "fragment-1"
	return store, now
}
