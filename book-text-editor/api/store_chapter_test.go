package api

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestMemoryStoreChapterCatalogAndScopedSnapshot(t *testing.T) {
	store := chapterStoreTestMemoryStore()
	ctx := context.Background()

	catalog, err := store.jobChapterCatalog(ctx, "job-1")
	if err != nil {
		t.Fatalf("jobChapterCatalog() error = %v", err)
	}
	wantCatalog := chapterCatalog{
		BookID: "book-1",
		Chapters: []chapterSummary{
			{
				Number:         1,
				Title:          "Начало",
				FragmentsCount: 2,
				DurationMS:     450,
			},
			{
				Number:         2,
				Title:          "Продолжение",
				FragmentsCount: 1,
				DurationMS:     300,
			},
		},
	}
	if !reflect.DeepEqual(catalog, wantCatalog) {
		t.Errorf("jobChapterCatalog() = %#v, want %#v", catalog, wantCatalog)
	}

	// A catalog is metadata-only, and exporting chapter 1 must not touch the
	// deliberately unavailable audio payload in chapter 2.
	store.fragments["fragment-2"].AudioPCM = nil
	chapter, err := store.chapterArchiveSnapshot(ctx, "job-1", 1)
	if err != nil {
		t.Fatalf("chapterArchiveSnapshot(chapter 1) error = %v", err)
	}
	if len(chapter.Fragments) != 2 ||
		chapter.Fragments[0].Resource.ID != "fragment-1" ||
		chapter.Fragments[1].Resource.ID != "fragment-3" {
		t.Fatalf(
			"chapterArchiveSnapshot(chapter 1) fragments = %#v",
			chapter.Fragments,
		)
	}

	storedAudio := slices.Clone(store.fragments["fragment-1"].AudioPCM)
	chapter.Fragments[0].AudioPCM[0] ^= 0xff
	if !bytes.Equal(store.fragments["fragment-1"].AudioPCM, storedAudio) {
		t.Error("chapter snapshot aliases stored audio")
	}

	if _, err := store.chapterArchiveSnapshot(
		ctx,
		"job-1",
		2,
	); !errors.Is(err, errConflict) {
		t.Fatalf(
			"chapterArchiveSnapshot(unready audio) error = %v, "+
				"want errConflict",
			err,
		)
	}
	if _, err := store.chapterArchiveSnapshot(
		ctx,
		"job-1",
		99,
	); !errors.Is(err, errChapterNotFound) {
		t.Fatalf(
			"chapterArchiveSnapshot(missing chapter) error = %v, "+
				"want errChapterNotFound",
			err,
		)
	}
}

func TestMemoryStoreChapterExportErrors(t *testing.T) {
	store := chapterStoreTestMemoryStore()
	ctx := context.Background()

	if _, err := store.jobChapterCatalog(
		ctx,
		"missing",
	); !errors.Is(err, errNotFound) {
		t.Errorf("jobChapterCatalog(missing) error = %v", err)
	}
	if _, err := store.chapterArchiveSnapshot(
		ctx,
		"missing",
		1,
	); !errors.Is(err, errNotFound) {
		t.Errorf("chapterArchiveSnapshot(missing job) error = %v", err)
	}
	if _, err := store.chapterArchiveSnapshot(
		ctx,
		"job-1",
		0,
	); !errors.Is(err, errInvalid) {
		t.Errorf("chapterArchiveSnapshot(invalid number) error = %v", err)
	}

	store.jobs["job-1"].Resource.Status = JobStatusRunning
	if _, err := store.jobChapterCatalog(
		ctx,
		"job-1",
	); !errors.Is(err, errConflict) {
		t.Errorf("jobChapterCatalog(running) error = %v", err)
	}
	if _, err := store.chapterArchiveSnapshot(
		ctx,
		"job-1",
		1,
	); !errors.Is(err, errConflict) {
		t.Errorf("chapterArchiveSnapshot(running) error = %v", err)
	}
}

func chapterStoreTestMemoryStore() *memoryStore {
	snapshot := chapterArchiveTestSnapshot()
	slices.SortFunc(snapshot.Fragments, func(left, right archiveFragment) int {
		return left.Resource.Ordinal - right.Resource.Ordinal
	})

	store := newMemoryStore()
	store.books[snapshot.Book.ID] = &bookRecord{
		Resource:    snapshot.Book,
		LatestJobID: snapshot.Job.ID,
	}
	store.voicesMap[snapshot.Voice.ID] = &voiceRecord{
		Payload: voicePayload{Resource: snapshot.Voice},
	}
	fragmentIDs := make([]string, 0, len(snapshot.Fragments))
	for _, fragment := range snapshot.Fragments {
		fragmentIDs = append(fragmentIDs, fragment.Resource.ID)
		store.fragments[fragment.Resource.ID] = &fragmentRecord{
			Resource:     fragment.Resource,
			ChapterTitle: fragment.ChapterTitle,
			BookID:       snapshot.Book.ID,
			VoiceID:      snapshot.Voice.ID,
			AudioPCM:     slices.Clone(fragment.AudioPCM),
			SampleRate:   fragment.SampleRate,
			Channels:     fragment.Channels,
			SampleWidth:  fragment.SampleWidth,
			DurationMS:   fragment.DurationMS,
			WorkerNotes:  slices.Clone(fragment.WorkerNotes),
		}
	}
	store.jobs[snapshot.Job.ID] = &jobRecord{
		Resource:    snapshot.Job,
		FragmentIDs: fragmentIDs,
	}

	return store
}
