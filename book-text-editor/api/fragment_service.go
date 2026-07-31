package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var errGenerationQueueFull = errors.New("generation queue is full")

// FragmentViewResource is a lightweight read model for the UI. PCM bytes are
// deliberately excluded so the catalog can be polled while generation runs.
type FragmentViewResource struct {
	FragmentResource
	ChapterTitle   string `json:"chapter_title"`
	DurationMS     int    `json:"duration_ms"`
	AudioAvailable bool   `json:"audio_available"`
	Editable       bool   `json:"editable"`
}

type jobFragmentSnapshot struct {
	Job       JobResource
	Fragments []FragmentViewResource
}

// fragmentWorkflowStore is the persistence port required by the fragment
// review use case. Memory and PostgreSQL adapters keep the text revision and
// retry state transition in one storage transaction/critical section.
type fragmentWorkflowStore interface {
	jobFragments(context.Context, string) (jobFragmentSnapshot, bool, error)
	editAndPrepareFragment(
		context.Context,
		string,
		string,
		string,
		time.Time,
	) (FragmentResource, jobTask, error)
	cancelRetry(context.Context, jobTask, time.Time) error
	downloadableChapterSnapshot(
		context.Context,
		string,
		int,
	) (archiveSnapshot, error)
}

type fragmentService struct {
	store   fragmentWorkflowStore
	enqueue func(jobTask) bool
}

func newFragmentService(
	store fragmentWorkflowStore,
	enqueue func(jobTask) bool,
) (*fragmentService, error) {
	if store == nil {
		return nil, errors.New("fragment workflow store is required")
	}
	if enqueue == nil {
		return nil, errors.New("fragment generation enqueue function is required")
	}
	return &fragmentService{store: store, enqueue: enqueue}, nil
}

func (s *fragmentService) catalog(
	ctx context.Context,
	jobID string,
) (jobFragmentSnapshot, bool, error) {
	return s.store.jobFragments(ctx, jobID)
}

func (s *fragmentService) chapterSnapshot(
	ctx context.Context,
	jobID string,
	chapterNumber int,
) (archiveSnapshot, error) {
	return s.store.downloadableChapterSnapshot(ctx, jobID, chapterNumber)
}

// editAndQueue atomically persists the new revision and marks the fragment as
// pending, then hands the task to the bounded runner. If the in-memory queue is
// full, cancelRetry restores a durable warning state with the edited text and
// no stale audio; a later explicit retry remains safe.
func (s *fragmentService) editAndQueue(
	ctx context.Context,
	fragmentID, text, revisionID string,
	now time.Time,
) (FragmentResource, error) {
	fragment, task, err := s.store.editAndPrepareFragment(
		ctx,
		fragmentID,
		text,
		revisionID,
		now,
	)
	if err != nil {
		return FragmentResource{}, err
	}
	if s.enqueue(task) {
		return fragment, nil
	}

	rollbackContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rollbackErr := s.store.cancelRetry(rollbackContext, task, now); rollbackErr != nil {
		return fragment, fmt.Errorf(
			"%w; restore retryable edited fragment: %v",
			errGenerationQueueFull,
			rollbackErr,
		)
	}
	fragment.Status = FragmentStatusWarning
	fragment.WarningCode = "text_edited"
	fragment.UpdatedAt = now
	return fragment, errGenerationQueueFull
}

func fragmentView(
	resource FragmentResource,
	chapterTitle string,
	durationMS int,
	audioAvailable bool,
) FragmentViewResource {
	return FragmentViewResource{
		FragmentResource: resource,
		ChapterTitle:     chapterTitle,
		DurationMS:       durationMS,
		AudioAvailable:   audioAvailable,
		Editable:         isEditableFragmentStatus(resource.Status),
	}
}

func isEditableFragmentStatus(status FragmentStatus) bool {
	switch status {
	case FragmentStatusReady, FragmentStatusWarning, FragmentStatusFailed:
		return true
	default:
		return false
	}
}
