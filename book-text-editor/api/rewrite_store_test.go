package api

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMemoryStoreRewriteSelectionSupportsBookAndExplicitScopes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	store := bookWideRewriteTestMemoryStore(now)

	resource, task, err := store.createRewriteTask(
		ctx,
		"rewrite-all",
		"job-1",
		nil,
		"qwen-test",
		"Сохрани всю фразу.",
		0.1,
		20,
		0.8,
		0,
		1.05,
		1_024,
		now,
	)
	if err != nil {
		t.Fatalf("createRewriteTask(all) error = %v", err)
	}
	wantAll := []string{"fragment-1", "fragment-2"}
	if !reflect.DeepEqual(task.FragmentIDs, wantAll) {
		t.Fatalf("all-warning fragment IDs = %#v, want %#v", task.FragmentIDs, wantAll)
	}
	if resource.FragmentsCount != 2 || resource.FragmentsPending != 2 {
		t.Fatalf("all-warning resource = %+v", resource)
	}
	if store.fragments[task.FragmentIDs[0]].Resource.ChapterNumber != 1 ||
		store.fragments[task.FragmentIDs[1]].Resource.ChapterNumber != 2 {
		t.Fatalf("all-warning selection does not span both chapters: %#v", task.FragmentIDs)
	}

	// Creation takes an immutable snapshot. A warning that appears afterwards
	// belongs to a later operation and must not silently expand this task.
	store.fragments["fragment-3"].Resource.Status = FragmentStatusWarning
	store.fragments["fragment-3"].Resource.WarningCode = "transcript_mismatch"
	snapshot, ok, err := store.rewriteTask(ctx, resource.ID)
	if err != nil || !ok {
		t.Fatalf("rewriteTask(snapshot) = (%+v, %v, %v)", snapshot, ok, err)
	}
	gotSnapshot := make([]string, 0, len(snapshot.Fragments))
	for _, fragment := range snapshot.Fragments {
		gotSnapshot = append(gotSnapshot, fragment.FragmentID)
	}
	if !reflect.DeepEqual(gotSnapshot, wantAll) {
		t.Fatalf("stored rewrite snapshot = %#v, want %#v", gotSnapshot, wantAll)
	}

	// State-idempotency rejects a duplicate operation while this book already
	// has a queued/running rewrite. It cannot enqueue duplicate LLM calls.
	_, _, err = store.createRewriteTask(
		ctx,
		"rewrite-duplicate",
		"job-1",
		nil,
		"qwen-test",
		"Сохрани всю фразу.",
		0.1,
		20,
		0.8,
		0,
		1.05,
		1_024,
		now.Add(time.Second),
	)
	if !errors.Is(err, errConflict) {
		t.Fatalf("duplicate active rewrite error = %v, want errConflict", err)
	}

	if err := store.deleteRewriteTask(ctx, resource.ID); err != nil {
		t.Fatalf("deleteRewriteTask() error = %v", err)
	}
	_, selectedTask, err := store.createRewriteTask(
		ctx,
		"rewrite-selected",
		"job-1",
		[]string{"fragment-2"},
		"qwen-test",
		"Сохрани всю фразу.",
		0.1,
		20,
		0.8,
		0,
		1.05,
		1_024,
		now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("createRewriteTask(explicit) error = %v", err)
	}
	if !reflect.DeepEqual(selectedTask.FragmentIDs, []string{"fragment-2"}) {
		t.Fatalf("explicit fragment IDs = %#v", selectedTask.FragmentIDs)
	}
}

func bookWideRewriteTestMemoryStore(now time.Time) *memoryStore {
	store := chapterStoreTestMemoryStore()
	// Normalize the shared archive fixture into regular book order: two
	// fragments in chapter 1 followed by one fragment in chapter 2.
	store.fragments["fragment-3"].Resource.Ordinal = 2
	store.fragments["fragment-2"].Resource.Ordinal = 3
	store.jobs["job-1"].FragmentIDs = []string{
		"fragment-1",
		"fragment-3",
		"fragment-2",
	}
	store.fragments["fragment-1"].Resource.Status = FragmentStatusWarning
	store.fragments["fragment-1"].Resource.WarningCode = "transcript_mismatch"
	store.fragments["fragment-1"].Resource.STTText = "Не совпало."
	store.fragments["fragment-2"].Resource.Status = FragmentStatusWarning
	store.fragments["fragment-2"].Resource.WarningCode = "transcript_mismatch"
	store.fragments["fragment-2"].Resource.STTText = "Тоже не совпало."
	recomputeJob(store.jobs["job-1"], store.fragments, now)
	return store
}
