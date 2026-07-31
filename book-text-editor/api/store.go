package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"book-text-editor/internal/book"
)

var (
	errNotFound         = errors.New("resource not found")
	errRevisionNotFound = errors.New("fragment revision not found")
	errChapterNotFound  = errors.New("chapter not found")
	errConflict         = errors.New("resource state conflict")
	errInvalid          = errors.New("invalid operation")
	errTooLarge         = errors.New("payload is too large")
)

type chapterSummary struct {
	Number         int
	Title          string
	FragmentsCount int
	DurationMS     int64
}

type chapterCatalog struct {
	BookID   string
	Chapters []chapterSummary
}

type repository interface {
	close()

	createBook(
		context.Context,
		string,
		book.Book,
		time.Time,
	) (BookResource, error)
	book(context.Context, string) (BookResource, bool, error)
	createVoice(
		context.Context,
		string,
		string,
		string,
		string,
		string,
		[]byte,
		time.Time,
	) (VoiceResource, error)
	voice(context.Context, string) (VoiceResource, bool, error)
	voices(context.Context) ([]VoiceResource, error)

	createJob(
		context.Context,
		string,
		string,
		string,
		[]string,
		[]string,
		GenerationSettings,
		time.Time,
	) (JobResource, jobTask, error)
	deleteJob(context.Context, string) error
	job(context.Context, string) (JobResource, bool, error)
	listJobs(context.Context, jobListFilter) (jobListPage, error)
	jobIssues(context.Context, string) ([]FragmentResource, bool, error)
	editFragment(
		context.Context,
		string,
		string,
		string,
		time.Time,
	) (FragmentResource, error)
	fragmentRevisions(
		context.Context,
		string,
	) (fragmentRevisionHistory, bool, error)
	restoreFragmentRevision(
		context.Context,
		string,
		string,
		string,
		string,
		time.Time,
	) (fragmentRevisionMutation, error)
	fragmentAudio(
		context.Context,
		string,
	) (fragmentAudioSnapshot, bool, error)
	approveFragment(
		context.Context,
		string,
		string,
		string,
		time.Time,
	) (fragmentApproval, error)
	fragmentManualReviews(
		context.Context,
		string,
	) ([]FragmentManualReviewResource, bool, error)
	prepareRetry(
		context.Context,
		string,
		[]string,
		time.Time,
	) (jobTask, error)
	cancelRetry(context.Context, jobTask, time.Time) error

	// Fragment review application port. These methods keep the new workflow
	// explicit at compile time instead of relying on runtime type assertions.
	jobFragments(
		context.Context,
		string,
	) (jobFragmentSnapshot, bool, error)
	editAndPrepareFragment(
		context.Context,
		string,
		string,
		string,
		time.Time,
	) (FragmentResource, jobTask, error)
	downloadableChapterSnapshot(
		context.Context,
		string,
		int,
	) (archiveSnapshot, error)

	startTask(context.Context, jobTask, time.Time) error
	workItem(context.Context, string) (fragmentWorkItem, error)
	startFragment(context.Context, string, time.Time) (FragmentResource, error)
	completeFragment(
		context.Context,
		string,
		fragmentResult,
		time.Time,
	) error
	prepareAutomaticWarningRetry(context.Context, string, time.Time) error
	failFragment(context.Context, string, string, time.Time) error
	finishTask(context.Context, string, time.Time) error

	createRewriteTask(
		context.Context,
		string,
		string,
		[]string,
		string,
		string,
		float64,
		int,
		float64,
		float64,
		float64,
		int,
		time.Time,
	) (RewriteTaskResource, rewriteTask, error)
	deleteRewriteTask(context.Context, string) error
	rewriteTask(
		context.Context,
		string,
	) (RewriteTaskResponse, bool, error)
	startRewriteTask(context.Context, string, time.Time) error
	startRewriteFragment(
		context.Context,
		string,
		string,
		time.Time,
	) (rewriteWorkItem, error)
	completeRewriteFragment(
		context.Context,
		string,
		string,
		string,
		string,
		RewriterResult,
		time.Time,
	) error
	failRewriteFragment(
		context.Context,
		string,
		string,
		string,
		time.Time,
	) error
	finishRewriteTask(context.Context, string, time.Time) error

	latestJobID(context.Context, string) (string, bool, error)
	jobChapterCatalog(context.Context, string) (chapterCatalog, error)
	archiveSnapshot(context.Context, string) (archiveSnapshot, error)
	chapterArchiveSnapshot(
		context.Context,
		string,
		int,
	) (archiveSnapshot, error)
}

type bookRecord struct {
	Resource    BookResource
	Chapters    []chapterRecord
	LatestJobID string
}

type voiceRecord struct {
	Payload voicePayload
}

type jobRecord struct {
	Resource    JobResource
	FragmentIDs []string
}

type fragmentRecord struct {
	Resource      FragmentResource
	ChapterTitle  string
	BookID        string
	VoiceID       string
	AudioPCM      []byte
	SampleRate    int
	Channels      int
	SampleWidth   int
	DurationMS    int
	STTLanguage   string
	WorkerNotes   []string
	History       []fragmentAttempt
	TextRevisions []FragmentTextRevisionResource
	ManualReviews []FragmentManualReviewResource
}

type fragmentAttempt struct {
	Attempt     int
	Text        string
	STTText     string
	Status      FragmentStatus
	WarningCode string
	Error       string
	AudioPCM    []byte
	SampleRate  int
	Channels    int
	SampleWidth int
	DurationMS  int
	STTLanguage string
	WorkerNotes []string
	CompletedAt time.Time
}

type memoryStore struct {
	mu              sync.RWMutex
	books           map[string]*bookRecord
	voicesMap       map[string]*voiceRecord
	jobs            map[string]*jobRecord
	fragments       map[string]*fragmentRecord
	textRevisionIDs map[string]string
	manualReviewIDs map[string]string
	rewriteTasks    map[string]*rewriteTaskRecord
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		books:           make(map[string]*bookRecord),
		voicesMap:       make(map[string]*voiceRecord),
		jobs:            make(map[string]*jobRecord),
		fragments:       make(map[string]*fragmentRecord),
		textRevisionIDs: make(map[string]string),
		manualReviewIDs: make(map[string]string),
		rewriteTasks:    make(map[string]*rewriteTaskRecord),
	}
}

func (*memoryStore) close() {}

func (s *memoryStore) createBook(
	_ context.Context,
	id string,
	parsed book.Book,
	now time.Time,
) (BookResource, error) {
	chapters := make([]chapterRecord, 0, len(parsed.Chapters))
	for _, chapter := range parsed.Chapters {
		chapters = append(chapters, chapterRecord{
			Number:   chapter.Number,
			Title:    chapter.Title,
			Segments: slices.Clone(chapter.Segments),
		})
	}
	resource := BookResource{
		ID:             id,
		Title:          parsed.Title,
		Authors:        slices.Clone(parsed.Authors),
		Format:         "fb2",
		ChaptersCount:  len(parsed.Chapters),
		FragmentsCount: parsed.SegmentCount(),
		Status:         "ready",
		CreatedAt:      now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.books[id]; exists {
		return BookResource{}, fmt.Errorf("%w: duplicate book id", errConflict)
	}
	s.books[id] = &bookRecord{
		Resource: resource,
		Chapters: chapters,
	}

	return cloneBookResource(resource), nil
}

func (s *memoryStore) book(
	_ context.Context,
	id string,
) (BookResource, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.books[id]
	if !ok {
		return BookResource{}, false, nil
	}

	return cloneBookResource(record.Resource), true, nil
}

func (s *memoryStore) createVoice(
	_ context.Context,
	id, name, format, contentType, referenceText string,
	audio []byte,
	now time.Time,
) (VoiceResource, error) {
	resource := VoiceResource{
		ID:               id,
		Name:             name,
		Mode:             "voice_clone",
		Format:           format,
		ContentType:      contentType,
		SizeBytes:        int64(len(audio)),
		HasReferenceText: referenceText != "",
		Status:           "ready",
		CreatedAt:        now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.voicesMap[id]; exists {
		return VoiceResource{}, fmt.Errorf("%w: duplicate voice id", errConflict)
	}
	s.voicesMap[id] = &voiceRecord{Payload: voicePayload{
		Resource:      resource,
		ReferenceText: referenceText,
		Audio:         slices.Clone(audio),
	}}

	return resource, nil
}

func (s *memoryStore) voice(
	_ context.Context,
	id string,
) (VoiceResource, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.voicesMap[id]
	if !ok {
		return VoiceResource{}, false, nil
	}

	return record.Payload.Resource, true, nil
}

func (s *memoryStore) voices(
	_ context.Context,
) ([]VoiceResource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]VoiceResource, 0, len(s.voicesMap))
	for _, record := range s.voicesMap {
		result = append(result, record.Payload.Resource)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].CreatedAt.Equal(result[right].CreatedAt) {
			return result[left].ID < result[right].ID
		}
		return result[left].CreatedAt.Before(result[right].CreatedAt)
	})

	return result, nil
}

func (s *memoryStore) createJob(
	_ context.Context,
	id, bookID, voiceID string,
	fragmentIDs, originalRevisionIDs []string,
	settings GenerationSettings,
	now time.Time,
) (JobResource, jobTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateGenerationSettings(settings); err != nil {
		return JobResource{}, jobTask{}, fmt.Errorf("%w: %v", errInvalid, err)
	}
	bookEntry, bookOK := s.books[bookID]
	if !bookOK {
		return JobResource{}, jobTask{}, fmt.Errorf("%w: book not found", errNotFound)
	}
	if _, voiceOK := s.voicesMap[voiceID]; !voiceOK {
		return JobResource{}, jobTask{}, fmt.Errorf("%w: voice not found", errNotFound)
	}
	if _, exists := s.jobs[id]; exists {
		return JobResource{}, jobTask{}, fmt.Errorf("%w: duplicate job id", errConflict)
	}
	if len(fragmentIDs) != bookEntry.Resource.FragmentsCount {
		return JobResource{}, jobTask{}, fmt.Errorf(
			"%w: got %d fragment ids for %d book fragments",
			errInvalid,
			len(fragmentIDs),
			bookEntry.Resource.FragmentsCount,
		)
	}
	if len(originalRevisionIDs) != len(fragmentIDs) {
		return JobResource{}, jobTask{}, fmt.Errorf(
			"%w: got %d original revision ids for %d fragments",
			errInvalid,
			len(originalRevisionIDs),
			len(fragmentIDs),
		)
	}
	if duplicate := firstDuplicate(fragmentIDs); duplicate != "" {
		return JobResource{}, jobTask{}, fmt.Errorf(
			"%w: duplicate fragment id %q",
			errInvalid,
			duplicate,
		)
	}
	for _, fragmentID := range fragmentIDs {
		if fragmentID == "" {
			return JobResource{}, jobTask{}, fmt.Errorf(
				"%w: fragment id is empty",
				errInvalid,
			)
		}
		if _, exists := s.fragments[fragmentID]; exists {
			return JobResource{}, jobTask{}, fmt.Errorf(
				"%w: duplicate fragment id %q",
				errConflict,
				fragmentID,
			)
		}
	}
	if duplicate := firstDuplicate(originalRevisionIDs); duplicate != "" {
		return JobResource{}, jobTask{}, fmt.Errorf(
			"%w: duplicate original revision id %q",
			errInvalid,
			duplicate,
		)
	}
	for _, revisionID := range originalRevisionIDs {
		if revisionID == "" {
			return JobResource{}, jobTask{}, fmt.Errorf(
				"%w: original revision id is empty",
				errInvalid,
			)
		}
		if _, exists := s.textRevisionIDs[revisionID]; exists {
			return JobResource{}, jobTask{}, fmt.Errorf(
				"%w: duplicate original revision id %q",
				errConflict,
				revisionID,
			)
		}
	}

	resource := JobResource{
		ID:                 id,
		BookID:             bookID,
		VoiceID:            voiceID,
		Status:             JobStatusQueued,
		FragmentsCount:     len(fragmentIDs),
		FragmentsPending:   len(fragmentIDs),
		GenerationSettings: settings,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	record := &jobRecord{
		Resource:    resource,
		FragmentIDs: slices.Clone(fragmentIDs),
	}
	s.jobs[id] = record
	bookEntry.LatestJobID = id

	globalOrdinal := 0
	for _, chapter := range bookEntry.Chapters {
		for _, text := range chapter.Segments {
			fragmentID := fragmentIDs[globalOrdinal]
			revisionID := originalRevisionIDs[globalOrdinal]
			globalOrdinal++
			originalRevision := newFragmentTextRevision(
				revisionID,
				fragmentID,
				1,
				FragmentTextRevisionSourceOriginal,
				text,
				"",
				"",
				now,
			)
			s.fragments[fragmentID] = &fragmentRecord{
				Resource: FragmentResource{
					ID:            fragmentID,
					JobID:         id,
					ChapterNumber: chapter.Number,
					Ordinal:       globalOrdinal,
					Text:          text,
					Status:        FragmentStatusPending,
					UpdatedAt:     now,
				},
				ChapterTitle: chapter.Title,
				BookID:       bookID,
				VoiceID:      voiceID,
				TextRevisions: []FragmentTextRevisionResource{
					originalRevision,
				},
			}
			s.textRevisionIDs[revisionID] = fragmentID
		}
	}

	return resource, jobTask{
		JobID:       id,
		FragmentIDs: slices.Clone(fragmentIDs),
		Settings:    settings,
	}, nil
}

func (s *memoryStore) deleteJob(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.jobs[id]
	if !ok {
		return nil
	}
	for _, fragmentID := range record.FragmentIDs {
		for _, revision := range s.fragments[fragmentID].TextRevisions {
			delete(s.textRevisionIDs, revision.ID)
		}
		for _, review := range s.fragments[fragmentID].ManualReviews {
			delete(s.manualReviewIDs, review.ID)
		}
		delete(s.fragments, fragmentID)
	}
	delete(s.jobs, id)
	if bookEntry := s.books[record.Resource.BookID]; bookEntry != nil &&
		bookEntry.LatestJobID == id {
		bookEntry.LatestJobID = ""
	}

	return nil
}

func (s *memoryStore) job(
	_ context.Context,
	id string,
) (JobResource, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.jobs[id]
	if !ok {
		return JobResource{}, false, nil
	}

	return record.Resource, true, nil
}

func (s *memoryStore) listJobs(
	_ context.Context,
	filter jobListFilter,
) (jobListPage, error) {
	if err := filter.validate(); err != nil {
		return jobListPage{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]JobResource, 0, len(s.jobs))
	for _, record := range s.jobs {
		if len(filter.Statuses) > 0 &&
			!slices.Contains(filter.Statuses, record.Resource.Status) {
			continue
		}
		result = append(result, record.Resource)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].CreatedAt.Equal(result[right].CreatedAt) {
			return result[left].ID > result[right].ID
		}
		return result[left].CreatedAt.After(result[right].CreatedAt)
	})

	total := int64(len(result))
	start := filter.Offset
	if start > len(result) {
		start = len(result)
	}
	end := start + filter.Limit
	if end > len(result) {
		end = len(result)
	}
	result = slices.Clone(result[start:end])
	if result == nil {
		result = make([]JobResource, 0)
	}

	return jobListPage{Jobs: result, Total: total}, nil
}

func (s *memoryStore) jobIssues(
	_ context.Context,
	jobID string,
) ([]FragmentResource, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	jobEntry, ok := s.jobs[jobID]
	if !ok {
		return nil, false, nil
	}

	result := make([]FragmentResource, 0)
	for _, id := range jobEntry.FragmentIDs {
		fragment := s.fragments[id]
		if fragment.Resource.Status == FragmentStatusWarning ||
			fragment.Resource.Status == FragmentStatusFailed {
			result = append(result, fragment.Resource)
		}
	}
	if result == nil {
		result = make([]FragmentResource, 0)
	}

	return result, true, nil
}

func (s *memoryStore) editFragment(
	_ context.Context,
	id, text, revisionID string,
	now time.Time,
) (FragmentResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.fragments[id]
	if !ok {
		return FragmentResource{}, fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if record.Resource.Status == FragmentStatusGenerating {
		return FragmentResource{}, fmt.Errorf(
			"%w: fragment is currently being processed",
			errConflict,
		)
	}
	if record.Resource.Status != FragmentStatusWarning &&
		record.Resource.Status != FragmentStatusFailed {
		return FragmentResource{}, fmt.Errorf(
			"%w: only warning or failed fragments can be edited",
			errConflict,
		)
	}
	if revisionID == "" {
		return FragmentResource{}, fmt.Errorf(
			"%w: revision id is empty",
			errInvalid,
		)
	}
	if _, exists := s.textRevisionIDs[revisionID]; exists {
		return FragmentResource{}, fmt.Errorf(
			"%w: duplicate revision id",
			errConflict,
		)
	}
	current, ok := currentTextRevision(record.TextRevisions)
	if !ok {
		return FragmentResource{}, errors.New(
			"fragment has no current text revision",
		)
	}

	record.Resource.Text = text
	record.Resource.STTText = ""
	record.Resource.Status = FragmentStatusWarning
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
	recomputeJob(s.jobs[record.Resource.JobID], s.fragments, now)

	return record.Resource, nil
}

func (s *memoryStore) prepareRetry(
	_ context.Context,
	jobID string,
	requested []string,
	now time.Time,
) (jobTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobEntry, ok := s.jobs[jobID]
	if !ok {
		return jobTask{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if jobEntry.Resource.Status == JobStatusQueued ||
		jobEntry.Resource.Status == JobStatusRunning {
		return jobTask{}, fmt.Errorf("%w: job is already active", errConflict)
	}
	for _, rewrite := range s.rewriteTasks {
		if rewrite.Resource.JobID == jobID &&
			(rewrite.Resource.Status == RewriteTaskStatusQueued ||
				rewrite.Resource.Status == RewriteTaskStatusRunning) {
			return jobTask{}, fmt.Errorf(
				"%w: job has an active text rewrite",
				errConflict,
			)
		}
	}

	selected := slices.Clone(requested)
	if len(selected) == 0 {
		for _, id := range jobEntry.FragmentIDs {
			status := s.fragments[id].Resource.Status
			if status == FragmentStatusWarning || status == FragmentStatusFailed {
				selected = append(selected, id)
			}
		}
	}
	if len(selected) == 0 {
		return jobTask{}, fmt.Errorf("%w: job has no retryable fragments", errConflict)
	}

	seen := make(map[string]struct{}, len(selected))
	previous := make(map[string]FragmentStatus, len(selected))
	for _, id := range selected {
		if _, duplicate := seen[id]; duplicate {
			return jobTask{}, fmt.Errorf("%w: duplicate fragment id %q", errInvalid, id)
		}
		seen[id] = struct{}{}

		fragment, exists := s.fragments[id]
		if !exists {
			return jobTask{}, fmt.Errorf("%w: fragment %q not found", errNotFound, id)
		}
		if fragment.Resource.JobID != jobID {
			return jobTask{}, fmt.Errorf(
				"%w: fragment %q does not belong to job",
				errInvalid,
				id,
			)
		}
		if fragment.Resource.Status != FragmentStatusWarning &&
			fragment.Resource.Status != FragmentStatusFailed {
			return jobTask{}, fmt.Errorf(
				"%w: fragment %q is not retryable",
				errInvalid,
				id,
			)
		}
		previous[id] = fragment.Resource.Status
	}

	for _, id := range selected {
		fragment := s.fragments[id]
		fragment.Resource.Status = FragmentStatusPending
		fragment.Resource.UpdatedAt = now
	}
	jobEntry.Resource.Status = JobStatusQueued
	jobEntry.Resource.UpdatedAt = now
	recomputeJob(jobEntry, s.fragments, now)
	jobEntry.Resource.Status = JobStatusQueued

	return jobTask{
		JobID:            jobID,
		FragmentIDs:      selected,
		IsRetry:          true,
		PreviousStatuses: previous,
		Settings:         jobEntry.Resource.GenerationSettings,
	}, nil
}

func (s *memoryStore) cancelRetry(
	_ context.Context,
	task jobTask,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobEntry, ok := s.jobs[task.JobID]
	if !ok {
		return nil
	}
	for id, status := range task.PreviousStatuses {
		if fragment := s.fragments[id]; fragment != nil &&
			fragment.Resource.Status == FragmentStatusPending {
			fragment.Resource.Status = status
			fragment.Resource.UpdatedAt = now
		}
	}
	recomputeJob(jobEntry, s.fragments, now)

	return nil
}

func (s *memoryStore) startTask(
	_ context.Context,
	task jobTask,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobEntry, ok := s.jobs[task.JobID]
	if !ok {
		return fmt.Errorf("%w: job not found", errNotFound)
	}
	jobEntry.Resource.Status = JobStatusRunning
	jobEntry.Resource.UpdatedAt = now

	return nil
}

func (s *memoryStore) workItem(
	_ context.Context,
	fragmentID string,
) (fragmentWorkItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fragment, ok := s.fragments[fragmentID]
	if !ok {
		return fragmentWorkItem{}, fmt.Errorf("%w: fragment not found", errNotFound)
	}
	voice := s.voicesMap[fragment.VoiceID]
	if voice == nil {
		return fragmentWorkItem{}, fmt.Errorf("%w: voice not found", errNotFound)
	}

	return fragmentWorkItem{
		Resource: fragment.Resource,
		Voice: voicePayload{
			Resource:      voice.Payload.Resource,
			ReferenceText: voice.Payload.ReferenceText,
			Audio:         slices.Clone(voice.Payload.Audio),
		},
	}, nil
}

func (s *memoryStore) startFragment(
	_ context.Context,
	fragmentID string,
	now time.Time,
) (FragmentResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fragment, ok := s.fragments[fragmentID]
	if !ok {
		return FragmentResource{}, fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if fragment.Resource.Status != FragmentStatusPending {
		return FragmentResource{}, fmt.Errorf(
			"%w: fragment is not pending",
			errConflict,
		)
	}
	fragment.Resource.Status = FragmentStatusGenerating
	fragment.Resource.Attempt++
	fragment.Resource.UpdatedAt = now
	fragment.Resource.WarningCode = ""
	fragment.Resource.Error = ""

	return fragment.Resource, nil
}

func (s *memoryStore) completeFragment(
	_ context.Context,
	fragmentID string,
	result fragmentResult,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fragment, ok := s.fragments[fragmentID]
	if !ok {
		return fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if fragment.Resource.Status != FragmentStatusGenerating {
		return fmt.Errorf("%w: fragment is not generating", errConflict)
	}

	fragment.Resource.STTText = result.STTText
	fragment.Resource.WarningCode = result.WarningCode
	fragment.Resource.Error = ""
	fragment.Resource.UpdatedAt = now
	if result.WarningCode == "" {
		fragment.Resource.Status = FragmentStatusReady
	} else {
		fragment.Resource.Status = FragmentStatusWarning
	}
	fragment.AudioPCM = slices.Clone(result.AudioPCM)
	fragment.SampleRate = result.SampleRate
	fragment.Channels = result.Channels
	fragment.SampleWidth = result.SampleWidth
	fragment.DurationMS = result.DurationMS
	fragment.STTLanguage = result.STTLanguage
	fragment.WorkerNotes = slices.Clone(result.WorkerNotes)
	fragment.History = append(fragment.History, snapshotAttempt(fragment, now))
	recomputeJob(s.jobs[fragment.Resource.JobID], s.fragments, now)

	return nil
}

func (s *memoryStore) failFragment(
	_ context.Context,
	fragmentID, message string,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fragment, ok := s.fragments[fragmentID]
	if !ok {
		return fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if fragment.Resource.Status != FragmentStatusGenerating {
		return fmt.Errorf("%w: fragment is not generating", errConflict)
	}
	fragment.Resource.Status = FragmentStatusFailed
	fragment.Resource.WarningCode = ""
	fragment.Resource.Error = message
	fragment.Resource.UpdatedAt = now
	fragment.History = append(fragment.History, snapshotAttempt(fragment, now))
	recomputeJob(s.jobs[fragment.Resource.JobID], s.fragments, now)

	return nil
}

func (s *memoryStore) finishTask(
	_ context.Context,
	jobID string,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobEntry, ok := s.jobs[jobID]
	if !ok {
		return fmt.Errorf("%w: job not found", errNotFound)
	}
	recomputeJob(jobEntry, s.fragments, now)

	return nil
}

func (s *memoryStore) latestJobID(
	_ context.Context,
	bookID string,
) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bookEntry, ok := s.books[bookID]
	if !ok || bookEntry.LatestJobID == "" {
		return "", false, nil
	}

	return bookEntry.LatestJobID, true, nil
}

func (s *memoryStore) archiveSnapshot(
	_ context.Context,
	jobID string,
) (archiveSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.archiveSnapshotLocked(jobID, 0)
}

func (s *memoryStore) chapterArchiveSnapshot(
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
	return s.archiveSnapshotLocked(jobID, chapterNumber)
}

func (s *memoryStore) jobChapterCatalog(
	_ context.Context,
	jobID string,
) (chapterCatalog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	jobEntry, ok := s.jobs[jobID]
	if !ok {
		return chapterCatalog{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if jobEntry.Resource.Status != JobStatusCompleted {
		return chapterCatalog{}, fmt.Errorf(
			"%w: all fragments must be ready before export",
			errConflict,
		)
	}
	if len(jobEntry.FragmentIDs) != jobEntry.Resource.FragmentsCount {
		return chapterCatalog{}, errors.New(
			"chapter catalog fragment count does not match job",
		)
	}

	byNumber := make(map[int]*chapterSummary)
	for _, id := range jobEntry.FragmentIDs {
		fragment := s.fragments[id]
		if fragment == nil ||
			fragment.Resource.Status != FragmentStatusReady {
			return chapterCatalog{}, fmt.Errorf(
				"%w: fragment metadata is not ready",
				errConflict,
			)
		}
		number := fragment.Resource.ChapterNumber
		if number <= 0 || fragment.DurationMS < 0 {
			return chapterCatalog{}, errors.New(
				"chapter catalog contains invalid fragment metadata",
			)
		}

		summary := byNumber[number]
		if summary == nil {
			summary = &chapterSummary{
				Number: number,
				Title:  fragment.ChapterTitle,
			}
			byNumber[number] = summary
		} else if summary.Title != fragment.ChapterTitle {
			return chapterCatalog{}, errors.New(
				"chapter catalog contains inconsistent titles",
			)
		}
		summary.FragmentsCount++
		summary.DurationMS += int64(fragment.DurationMS)
	}

	chapters := make([]chapterSummary, 0, len(byNumber))
	for _, summary := range byNumber {
		chapters = append(chapters, *summary)
	}
	sort.Slice(chapters, func(left, right int) bool {
		return chapters[left].Number < chapters[right].Number
	})

	return chapterCatalog{
		BookID:   jobEntry.Resource.BookID,
		Chapters: chapters,
	}, nil
}

func (s *memoryStore) archiveSnapshotLocked(
	jobID string,
	chapterNumber int,
) (archiveSnapshot, error) {
	jobEntry, ok := s.jobs[jobID]
	if !ok {
		return archiveSnapshot{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if jobEntry.Resource.Status != JobStatusCompleted {
		return archiveSnapshot{}, fmt.Errorf(
			"%w: all fragments must be ready before export",
			errConflict,
		)
	}
	bookEntry := s.books[jobEntry.Resource.BookID]
	voiceEntry := s.voicesMap[jobEntry.Resource.VoiceID]
	if bookEntry == nil || voiceEntry == nil {
		return archiveSnapshot{}, errors.New("archive references missing resources")
	}

	fragmentCapacity := len(jobEntry.FragmentIDs)
	if chapterNumber > 0 {
		fragmentCapacity = 0
	}
	snapshot := archiveSnapshot{
		Book:      cloneBookResource(bookEntry.Resource),
		Voice:     voiceEntry.Payload.Resource,
		Job:       jobEntry.Resource,
		Fragments: make([]archiveFragment, 0, fragmentCapacity),
	}
	for _, id := range jobEntry.FragmentIDs {
		fragment := s.fragments[id]
		if fragment == nil {
			return archiveSnapshot{}, errors.New(
				"archive references a missing fragment",
			)
		}
		if chapterNumber > 0 &&
			fragment.Resource.ChapterNumber != chapterNumber {
			continue
		}
		if fragment.Resource.Status != FragmentStatusReady ||
			len(fragment.AudioPCM) == 0 {
			return archiveSnapshot{}, fmt.Errorf(
				"%w: fragment audio is not ready",
				errConflict,
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
	if chapterNumber > 0 && len(snapshot.Fragments) == 0 {
		return archiveSnapshot{}, fmt.Errorf(
			"%w: chapter %d has no generated fragments",
			errChapterNotFound,
			chapterNumber,
		)
	}
	if chapterNumber == 0 &&
		len(snapshot.Fragments) != len(jobEntry.FragmentIDs) {
		return archiveSnapshot{}, errors.New(
			"archive fragment count does not match job",
		)
	}

	return snapshot, nil
}

func recomputeJob(
	job *jobRecord,
	fragments map[string]*fragmentRecord,
	now time.Time,
) {
	resource := &job.Resource
	resource.FragmentsPending = 0
	resource.FragmentsReady = 0
	resource.FragmentsWarnings = 0
	resource.FragmentsFailed = 0
	active := false

	for _, id := range job.FragmentIDs {
		switch fragments[id].Resource.Status {
		case FragmentStatusPending:
			resource.FragmentsPending++
		case FragmentStatusGenerating:
			resource.FragmentsPending++
			active = true
		case FragmentStatusReady:
			resource.FragmentsReady++
		case FragmentStatusWarning:
			resource.FragmentsWarnings++
		case FragmentStatusFailed:
			resource.FragmentsFailed++
		}
	}

	switch {
	case active || resource.FragmentsPending > 0:
		resource.Status = JobStatusRunning
	case resource.FragmentsFailed > 0:
		resource.Status = JobStatusFailed
	case resource.FragmentsWarnings > 0:
		resource.Status = JobStatusCompletedWithWarnings
	default:
		resource.Status = JobStatusCompleted
	}
	resource.UpdatedAt = now
}

func snapshotAttempt(
	fragment *fragmentRecord,
	now time.Time,
) fragmentAttempt {
	return fragmentAttempt{
		Attempt:     fragment.Resource.Attempt,
		Text:        fragment.Resource.Text,
		STTText:     fragment.Resource.STTText,
		Status:      fragment.Resource.Status,
		WarningCode: fragment.Resource.WarningCode,
		Error:       fragment.Resource.Error,
		AudioPCM:    slices.Clone(fragment.AudioPCM),
		SampleRate:  fragment.SampleRate,
		Channels:    fragment.Channels,
		SampleWidth: fragment.SampleWidth,
		DurationMS:  fragment.DurationMS,
		STTLanguage: fragment.STTLanguage,
		WorkerNotes: slices.Clone(fragment.WorkerNotes),
		CompletedAt: now,
	}
}

func cloneBookResource(resource BookResource) BookResource {
	resource.Authors = slices.Clone(resource.Authors)
	return resource
}
