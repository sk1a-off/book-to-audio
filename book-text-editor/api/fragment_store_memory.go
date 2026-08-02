package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

func (s *memoryStore) jobFragments(
	_ context.Context,
	jobID string,
) (jobFragmentSnapshot, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	jobEntry, ok := s.jobs[jobID]
	if !ok {
		return jobFragmentSnapshot{}, false, nil
	}
	result := make([]FragmentViewResource, 0, len(jobEntry.FragmentIDs))
	for _, fragmentID := range jobEntry.FragmentIDs {
		record := s.fragments[fragmentID]
		if record == nil {
			return jobFragmentSnapshot{}, false, errors.New(
				"job references a missing fragment",
			)
		}
		result = append(result, fragmentView(
			record.Resource,
			record.ChapterTitle,
			record.DurationMS,
			len(record.AudioPCM) > 0,
		))
	}
	return jobFragmentSnapshot{
		Job:       jobEntry.Resource,
		Fragments: result,
	}, true, nil
}

func (s *memoryStore) editAndPrepareFragment(
	_ context.Context,
	id, text, revisionID string,
	now time.Time,
) (FragmentResource, jobTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.fragments[id]
	if !ok {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	if !isEditableFragmentStatus(record.Resource.Status) {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: only ready, warning or failed fragments can be edited",
			errConflict,
		)
	}
	for _, rewrite := range s.rewriteTasks {
		if rewrite.Resource.JobID == record.Resource.JobID &&
			(rewrite.Resource.Status == RewriteTaskStatusQueued ||
				rewrite.Resource.Status == RewriteTaskStatusRunning) {
			return FragmentResource{}, jobTask{}, fmt.Errorf(
				"%w: job has an active text rewrite",
				errConflict,
			)
		}
	}
	if revisionID == "" {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: revision id is empty",
			errInvalid,
		)
	}
	if _, exists := s.textRevisionIDs[revisionID]; exists {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: duplicate revision id",
			errConflict,
		)
	}
	current, ok := currentTextRevision(record.TextRevisions)
	if !ok {
		return FragmentResource{}, jobTask{}, errors.New(
			"fragment has no current text revision",
		)
	}
	jobEntry := s.jobs[record.Resource.JobID]
	if jobEntry == nil {
		return FragmentResource{}, jobTask{}, errors.New(
			"fragment references a missing job",
		)
	}
	if jobEntry.Resource.Status == JobStatusPaused ||
		jobEntry.Resource.Status == JobStatusCanceled {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: paused or canceled jobs cannot queue fragment edits",
			errConflict,
		)
	}
	wasActive := jobEntry.Resource.Status == JobStatusQueued ||
		jobEntry.Resource.Status == JobStatusRunning

	record.Resource.Text = text
	record.Resource.STTText = ""
	record.Resource.Status = FragmentStatusPending
	record.Resource.WarningCode = "text_edited"
	record.Resource.Error = ""
	record.Resource.UpdatedAt = now
	clearMemoryFragmentAudio(record)
	record.TextRevisions = append(
		record.TextRevisions,
		newFragmentTextRevision(
			revisionID,
			id,
			current.RevisionNumber+1,
			FragmentTextRevisionSourceManual,
			text,
			current.ID,
			"",
			now,
		),
	)
	s.textRevisionIDs[revisionID] = id
	recomputeJob(jobEntry, s.fragments, now)
	if !wasActive {
		jobEntry.Resource.Status = JobStatusQueued
		jobEntry.Resource.UpdatedAt = now
	}

	return record.Resource, jobTask{
		JobID:       jobEntry.Resource.ID,
		FragmentIDs: []string{id},
		IsRetry:     true,
		PreviousStatuses: map[string]FragmentStatus{
			id: FragmentStatusWarning,
		},
		Settings: jobEntry.Resource.GenerationSettings,
	}, nil
}

func (s *memoryStore) downloadableChapterSnapshot(
	_ context.Context,
	jobID string,
	chapterNumber int,
) (archiveSnapshot, error) {
	if chapterNumber <= 0 {
		return archiveSnapshot{}, fmt.Errorf(
			"%w: chapter number must be positive",
			errInvalid,
		)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	jobEntry, ok := s.jobs[jobID]
	if !ok {
		return archiveSnapshot{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	bookEntry := s.books[jobEntry.Resource.BookID]
	voiceEntry := s.voicesMap[jobEntry.Resource.VoiceID]
	if bookEntry == nil || voiceEntry == nil {
		return archiveSnapshot{}, errors.New("chapter references missing resources")
	}

	snapshot := archiveSnapshot{
		Book:      cloneBookResource(bookEntry.Resource),
		Voice:     voiceEntry.Payload.Resource,
		Job:       jobEntry.Resource,
		Fragments: make([]archiveFragment, 0),
	}
	for _, fragmentID := range jobEntry.FragmentIDs {
		fragment := s.fragments[fragmentID]
		if fragment == nil {
			return archiveSnapshot{}, errors.New("job references a missing fragment")
		}
		if fragment.Resource.ChapterNumber != chapterNumber {
			continue
		}
		if len(fragment.AudioPCM) == 0 {
			return archiveSnapshot{}, fmt.Errorf(
				"%w: chapter %d still contains unfinished fragments",
				errConflict,
				chapterNumber,
			)
		}
		snapshot.Fragments = append(snapshot.Fragments, archiveFragment{
			Resource:     fragment.Resource,
			ChapterTitle: fragment.ChapterTitle,
			AudioPCM:     slices.Clone(fragment.AudioPCM),
			SampleRate:   fragment.SampleRate,
			Channels:     fragment.Channels,
			SampleWidth:  fragment.SampleWidth,
			DurationMS:   fragment.DurationMS,
			WorkerNotes:  slices.Clone(fragment.WorkerNotes),
		})
	}
	if len(snapshot.Fragments) == 0 {
		return archiveSnapshot{}, fmt.Errorf(
			"%w: chapter %d has no generated fragments",
			errChapterNotFound,
			chapterNumber,
		)
	}
	return snapshot, nil
}

var _ fragmentWorkflowStore = (*memoryStore)(nil)
