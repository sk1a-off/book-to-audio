package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fragmentServiceStoreStub struct {
	fragment     FragmentResource
	task         jobTask
	editErr      error
	cancelErr    error
	cancelled    bool
	catalog      jobFragmentSnapshot
	catalogFound bool
	chapter      archiveSnapshot
	chapterErr   error
}

func (s *fragmentServiceStoreStub) jobFragments(
	context.Context,
	string,
) (jobFragmentSnapshot, bool, error) {
	return s.catalog, s.catalogFound, nil
}

func (s *fragmentServiceStoreStub) editAndPrepareFragment(
	context.Context,
	string,
	string,
	string,
	time.Time,
) (FragmentResource, jobTask, error) {
	return s.fragment, s.task, s.editErr
}

func (s *fragmentServiceStoreStub) cancelRetry(
	context.Context,
	jobTask,
	time.Time,
) error {
	s.cancelled = true
	return s.cancelErr
}

func (s *fragmentServiceStoreStub) downloadableChapterSnapshot(
	context.Context,
	string,
	int,
) (archiveSnapshot, error) {
	return s.chapter, s.chapterErr
}

func TestFragmentServiceEditAndQueue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 31, 10, 0, 0, 0, time.UTC)
	store := &fragmentServiceStoreStub{
		fragment: FragmentResource{
			ID: "fragment-1", JobID: "job-1", Status: FragmentStatusPending,
		},
		task: jobTask{JobID: "job-1", FragmentIDs: []string{"fragment-1"}},
	}
	service := &fragmentService{
		store: store,
		enqueue: func(task jobTask) bool {
			return task.JobID == "job-1" && len(task.FragmentIDs) == 1
		},
	}

	fragment, err := service.editAndQueue(
		context.Background(),
		"fragment-1",
		"Исправленный текст.",
		"revision-2",
		now,
	)
	if err != nil {
		t.Fatalf("editAndQueue() error = %v", err)
	}
	if fragment.Status != FragmentStatusPending || store.cancelled {
		t.Fatalf("fragment = %+v, cancelled = %t", fragment, store.cancelled)
	}
}

func TestFragmentServiceRestoresRetryableStateWhenQueueIsFull(t *testing.T) {
	t.Parallel()

	store := &fragmentServiceStoreStub{
		fragment: FragmentResource{
			ID: "fragment-1", JobID: "job-1", Status: FragmentStatusPending,
		},
		task: jobTask{
			JobID: "job-1",
			PreviousStatuses: map[string]FragmentStatus{
				"fragment-1": FragmentStatusWarning,
			},
		},
	}
	service := &fragmentService{store: store, enqueue: func(jobTask) bool { return false }}

	fragment, err := service.editAndQueue(
		context.Background(),
		"fragment-1",
		"Исправленный текст.",
		"revision-2",
		time.Now(),
	)
	if !errors.Is(err, errGenerationQueueFull) {
		t.Fatalf("editAndQueue() error = %v", err)
	}
	if !store.cancelled {
		t.Fatal("cancelRetry() was not called")
	}
	if fragment.Status != FragmentStatusWarning || fragment.WarningCode != "text_edited" {
		t.Fatalf("fragment = %+v", fragment)
	}
}
