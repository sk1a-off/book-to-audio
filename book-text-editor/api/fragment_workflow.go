package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// FragmentViewResource is the read model used by the UI while a job is still
// running. It deliberately contains no PCM bytes.
type FragmentViewResource struct {
	FragmentResource
	ChapterTitle   string `json:"chapter_title"`
	DurationMS     int    `json:"duration_ms"`
	AudioAvailable bool   `json:"audio_available"`
	Editable       bool   `json:"editable"`
}

type jobFragmentSnapshot struct {
	BookID    string
	Fragments []FragmentViewResource
}

type jobFragmentReader interface {
	jobFragments(context.Context, string) (jobFragmentSnapshot, bool, error)
}

type completedFragmentEditor interface {
	editCompletedFragment(
		context.Context,
		string,
		string,
		string,
		time.Time,
	) (FragmentResource, error)
}

type downloadableChapterReader interface {
	downloadableChapterSnapshot(context.Context, string, int) (archiveSnapshot, error)
}

type editedFragmentPreparer interface {
	prepareEditedFragment(
		context.Context,
		string,
		time.Time,
	) (jobTask, error)
}

// repositoryFacade keeps legacy repository methods stable and adds focused
// capabilities needed by the fragment review workflow.
type repositoryFacade struct {
	repository
}

func newRepositoryFacade(base repository) repository {
	if _, ok := base.(*repositoryFacade); ok {
		return base
	}
	return &repositoryFacade{repository: base}
}

func (f *repositoryFacade) editFragment(
	ctx context.Context,
	id, text, revisionID string,
	now time.Time,
) (FragmentResource, error) {
	if editor, ok := f.repository.(completedFragmentEditor); ok {
		return editor.editCompletedFragment(ctx, id, text, revisionID, now)
	}
	return f.repository.editFragment(ctx, id, text, revisionID, now)
}

func (f *repositoryFacade) jobFragments(
	ctx context.Context,
	jobID string,
) (jobFragmentSnapshot, bool, error) {
	reader, ok := f.repository.(jobFragmentReader)
	if !ok {
		return jobFragmentSnapshot{}, false, errors.New(
			"repository does not support fragment catalog reads",
		)
	}
	return reader.jobFragments(ctx, jobID)
}

func (f *repositoryFacade) chapterArchiveSnapshot(
	ctx context.Context,
	jobID string,
	chapterNumber int,
) (archiveSnapshot, error) {
	reader, ok := f.repository.(downloadableChapterReader)
	if !ok {
		return f.repository.chapterArchiveSnapshot(ctx, jobID, chapterNumber)
	}
	return reader.downloadableChapterSnapshot(ctx, jobID, chapterNumber)
}

func (f *repositoryFacade) prepareEditedFragment(
	ctx context.Context,
	fragmentID string,
	now time.Time,
) (jobTask, error) {
	preparer, ok := f.repository.(editedFragmentPreparer)
	if !ok {
		return jobTask{}, errors.New(
			"repository does not support edited fragment regeneration",
		)
	}
	return preparer.prepareEditedFragment(ctx, fragmentID, now)
}

func (s *Server) queueEditedFragment(
	ctx context.Context,
	fragment FragmentResource,
	now time.Time,
) (FragmentResource, bool, error) {
	preparer, ok := s.store.(editedFragmentPreparer)
	if !ok {
		return fragment, false, nil
	}
	task, err := preparer.prepareEditedFragment(ctx, fragment.ID, now)
	if err != nil {
		return fragment, false, err
	}
	if !s.runner.enqueue(task) {
		if cancelErr := s.store.cancelRetry(ctx, task, now); cancelErr != nil {
			return fragment, false, fmt.Errorf(
				"generation queue is full; rollback edited fragment: %w",
				cancelErr,
			)
		}
		return fragment, false, errors.New("generation queue is full")
	}
	fragment.Status = FragmentStatusPending
	fragment.UpdatedAt = now
	return fragment, true, nil
}

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
		BookID:    jobEntry.Resource.BookID,
		Fragments: result,
	}, true, nil
}

func (s *memoryStore) editCompletedFragment(
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
	if !isEditableFragmentStatus(record.Resource.Status) {
		return FragmentResource{}, fmt.Errorf(
			"%w: only voiced, warning or failed fragments can be edited",
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

func (s *memoryStore) prepareEditedFragment(
	_ context.Context,
	fragmentID string,
	now time.Time,
) (jobTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fragment, ok := s.fragments[fragmentID]
	if !ok {
		return jobTask{}, fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if fragment.Resource.Status != FragmentStatusWarning &&
		fragment.Resource.Status != FragmentStatusFailed {
		return jobTask{}, fmt.Errorf(
			"%w: edited fragment is not ready for regeneration",
			errConflict,
		)
	}
	jobEntry := s.jobs[fragment.Resource.JobID]
	if jobEntry == nil {
		return jobTask{}, errors.New("fragment references a missing job")
	}
	wasActive := jobEntry.Resource.Status == JobStatusQueued ||
		jobEntry.Resource.Status == JobStatusRunning
	previous := fragment.Resource.Status
	fragment.Resource.Status = FragmentStatusPending
	fragment.Resource.UpdatedAt = now
	recomputeJob(jobEntry, s.fragments, now)
	if !wasActive {
		jobEntry.Resource.Status = JobStatusQueued
		jobEntry.Resource.UpdatedAt = now
	}
	return jobTask{
		JobID:       jobEntry.Resource.ID,
		FragmentIDs: []string{fragmentID},
		IsRetry:     true,
		PreviousStatuses: map[string]FragmentStatus{
			fragmentID: previous,
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
		if fragment.Resource.Status != FragmentStatusReady ||
			len(fragment.AudioPCM) == 0 {
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

func (s *PostgresStore) downloadableChapterSnapshot(
	ctx context.Context,
	jobID string,
	chapterNumber int,
) (archiveSnapshot, error) {
	if chapterNumber <= 0 {
		return archiveSnapshot{}, fmt.Errorf(
			"%w: chapter number must be positive",
			errInvalid,
		)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf(
			"begin chapter snapshot transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return archiveSnapshot{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf("query chapter job: %w", err)
	}
	bookResource, err := scanBook(
		tx.QueryRow(ctx, bookSelect+` WHERE id = $1`, job.BookID),
	)
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf("query chapter book: %w", err)
	}
	voiceResource, err := scanVoice(
		tx.QueryRow(ctx, voiceSelect+` WHERE id = $1`, job.VoiceID),
	)
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf("query chapter voice: %w", err)
	}

	rows, err := tx.Query(
		ctx,
		`SELECT
			id, job_id, chapter_number, ordinal, text, stt_text, status,
			warning_code, error_message, attempt, updated_at,
			chapter_title, audio_pcm, sample_rate, channels, sample_width,
			duration_ms, worker_notes
		 FROM job_fragments
		 WHERE job_id = $1 AND chapter_number = $2
		 ORDER BY ordinal ASC, id ASC`,
		jobID,
		chapterNumber,
	)
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf("query chapter fragments: %w", err)
	}
	defer rows.Close()

	fragments := make([]archiveFragment, 0)
	for rows.Next() {
		var fragment archiveFragment
		if err := rows.Scan(
			&fragment.Resource.ID,
			&fragment.Resource.JobID,
			&fragment.Resource.ChapterNumber,
			&fragment.Resource.Ordinal,
			&fragment.Resource.Text,
			&fragment.Resource.STTText,
			&fragment.Resource.Status,
			&fragment.Resource.WarningCode,
			&fragment.Resource.Error,
			&fragment.Resource.Attempt,
			&fragment.Resource.UpdatedAt,
			&fragment.ChapterTitle,
			&fragment.AudioPCM,
			&fragment.SampleRate,
			&fragment.Channels,
			&fragment.SampleWidth,
			&fragment.DurationMS,
			&fragment.WorkerNotes,
		); err != nil {
			return archiveSnapshot{}, fmt.Errorf("scan chapter fragment: %w", err)
		}
		if fragment.Resource.Status != FragmentStatusReady ||
			len(fragment.AudioPCM) == 0 {
			return archiveSnapshot{}, fmt.Errorf(
				"%w: chapter %d still contains unfinished fragments",
				errConflict,
				chapterNumber,
			)
		}
		fragment.AudioPCM = slices.Clone(fragment.AudioPCM)
		fragment.WorkerNotes = nonNilStrings(fragment.WorkerNotes)
		fragments = append(fragments, fragment)
	}
	if err := rows.Err(); err != nil {
		return archiveSnapshot{}, fmt.Errorf("iterate chapter fragments: %w", err)
	}
	if len(fragments) == 0 {
		return archiveSnapshot{}, fmt.Errorf(
			"%w: chapter %d has no generated fragments",
			errChapterNotFound,
			chapterNumber,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return archiveSnapshot{}, fmt.Errorf("commit chapter snapshot: %w", err)
	}
	return archiveSnapshot{
		Book:      bookResource,
		Voice:     voiceResource,
		Job:       job,
		Fragments: fragments,
	}, nil
}

func (s *PostgresStore) jobFragments(
	ctx context.Context,
	jobID string,
) (jobFragmentSnapshot, bool, error) {
	var bookID string
	if err := s.pool.QueryRow(
		ctx,
		`SELECT book_id FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&bookID); errors.Is(err, pgx.ErrNoRows) {
		return jobFragmentSnapshot{}, false, nil
	} else if err != nil {
		return jobFragmentSnapshot{}, false, fmt.Errorf("query fragment job: %w", err)
	}

	rows, err := s.pool.Query(
		ctx,
		`SELECT
			jf.id, jf.job_id, jf.chapter_number, jf.ordinal,
			jf.text, jf.stt_text, jf.status, jf.warning_code,
			jf.error_message, jf.attempt, jf.updated_at,
			jf.chapter_title, jf.duration_ms,
			COALESCE(octet_length(jf.audio_pcm), 0) > 0
		 FROM job_fragments AS jf
		 WHERE jf.job_id = $1
		 ORDER BY jf.ordinal ASC, jf.id ASC`,
		jobID,
	)
	if err != nil {
		return jobFragmentSnapshot{}, false, fmt.Errorf("query job fragments: %w", err)
	}
	defer rows.Close()

	result := make([]FragmentViewResource, 0)
	for rows.Next() {
		var (
			resource       FragmentResource
			chapterTitle   string
			durationMS     int
			audioAvailable bool
		)
		if err := rows.Scan(
			&resource.ID,
			&resource.JobID,
			&resource.ChapterNumber,
			&resource.Ordinal,
			&resource.Text,
			&resource.STTText,
			&resource.Status,
			&resource.WarningCode,
			&resource.Error,
			&resource.Attempt,
			&resource.UpdatedAt,
			&chapterTitle,
			&durationMS,
			&audioAvailable,
		); err != nil {
			return jobFragmentSnapshot{}, false, fmt.Errorf(
				"scan job fragment: %w",
				err,
			)
		}
		result = append(
			result,
			fragmentView(resource, chapterTitle, durationMS, audioAvailable),
		)
	}
	if err := rows.Err(); err != nil {
		return jobFragmentSnapshot{}, false, fmt.Errorf(
			"iterate job fragments: %w",
			err,
		)
	}
	return jobFragmentSnapshot{BookID: bookID, Fragments: result}, true, nil
}

func (s *PostgresStore) editCompletedFragment(
	ctx context.Context,
	id, text, revisionID string,
	now time.Time,
) (FragmentResource, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return FragmentResource{}, fmt.Errorf(
			"begin edit fragment transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	jobID, err := fragmentJobID(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return FragmentResource{}, fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return FragmentResource{}, fmt.Errorf("query fragment job: %w", err)
	}
	if err := lockJob(ctx, tx, jobID); err != nil {
		return FragmentResource{}, err
	}
	resource, err := scanFragment(
		tx.QueryRow(ctx, fragmentSelect+` WHERE id = $1 FOR UPDATE`, id),
	)
	if err != nil {
		return FragmentResource{}, fmt.Errorf("lock fragment: %w", err)
	}
	if !isEditableFragmentStatus(resource.Status) {
		return FragmentResource{}, fmt.Errorf(
			"%w: only voiced, warning or failed fragments can be edited",
			errConflict,
		)
	}
	if revisionID == "" {
		return FragmentResource{}, fmt.Errorf(
			"%w: revision id is empty",
			errInvalid,
		)
	}

	currentRevision, err := scanTextRevision(
		tx.QueryRow(
			ctx,
			textRevisionSelect+`
			 WHERE fragment_id = $1
			 ORDER BY revision_number DESC, id DESC
			 LIMIT 1
			 FOR UPDATE`,
			id,
		),
	)
	if err != nil {
		return FragmentResource{}, fmt.Errorf(
			"query current fragment revision: %w",
			err,
		)
	}
	if err := insertTextRevision(
		ctx,
		tx,
		newFragmentTextRevision(
			revisionID,
			id,
			currentRevision.RevisionNumber+1,
			FragmentTextRevisionSourceManual,
			text,
			currentRevision.ID,
			"",
			now,
		),
	); err != nil {
		return FragmentResource{}, err
	}

	resource.Text = text
	resource.STTText = ""
	resource.Status = FragmentStatusWarning
	resource.WarningCode = "text_edited"
	resource.Error = ""
	resource.UpdatedAt = now
	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments
		 SET text = $2,
		     stt_text = '',
		     status = $3,
		     warning_code = $4,
		     error_message = '',
		     audio_pcm = ''::BYTEA,
		     sample_rate = 0,
		     channels = 0,
		     sample_width = 0,
		     duration_ms = 0,
		     stt_language = '',
		     worker_notes = ARRAY[]::TEXT[],
		     updated_at = $5
		 WHERE id = $1`,
		id,
		resource.Text,
		resource.Status,
		resource.WarningCode,
		resource.UpdatedAt,
	)
	if err != nil {
		return FragmentResource{}, mapPostgresWriteError("update fragment", err)
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return FragmentResource{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FragmentResource{}, fmt.Errorf(
			"commit edit fragment transaction: %w",
			err,
		)
	}
	return resource, nil
}

func (s *PostgresStore) prepareEditedFragment(
	ctx context.Context,
	fragmentID string,
	now time.Time,
) (jobTask, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return jobTask{}, fmt.Errorf(
			"begin prepare edited fragment transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	fragment, err := scanFragment(
		tx.QueryRow(
			ctx,
			fragmentSelect+` WHERE id = $1 FOR UPDATE`,
			fragmentID,
		),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobTask{}, fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return jobTask{}, fmt.Errorf("lock edited fragment: %w", err)
	}
	job, err := scanJob(
		tx.QueryRow(
			ctx,
			jobSelect+` WHERE id = $1 FOR UPDATE`,
			fragment.JobID,
		),
	)
	if err != nil {
		return jobTask{}, fmt.Errorf("lock edited fragment job: %w", err)
	}
	if fragment.Status != FragmentStatusWarning &&
		fragment.Status != FragmentStatusFailed {
		return jobTask{}, fmt.Errorf(
			"%w: edited fragment is not ready for regeneration",
			errConflict,
		)
	}

	previous := fragment.Status
	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments
		 SET status = $2, updated_at = $3
		 WHERE id = $1`,
		fragmentID,
		FragmentStatusPending,
		now,
	)
	if err != nil {
		return jobTask{}, mapPostgresWriteError(
			"queue edited fragment",
			err,
		)
	}
	if err := recomputePostgresJob(ctx, tx, fragment.JobID, now); err != nil {
		return jobTask{}, err
	}
	if job.Status != JobStatusQueued && job.Status != JobStatusRunning {
		_, err = tx.Exec(
			ctx,
			`UPDATE jobs SET status = $2, updated_at = $3 WHERE id = $1`,
			fragment.JobID,
			JobStatusQueued,
			now,
		)
		if err != nil {
			return jobTask{}, fmt.Errorf("mark edited fragment job queued: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return jobTask{}, fmt.Errorf(
			"commit edited fragment regeneration: %w",
			err,
		)
	}
	return jobTask{
		JobID:       fragment.JobID,
		FragmentIDs: []string{fragmentID},
		IsRetry:     true,
		PreviousStatuses: map[string]FragmentStatus{
			fragmentID: previous,
		},
		Settings: job.GenerationSettings,
	}, nil
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
	return slices.Contains(
		[]FragmentStatus{
			FragmentStatusReady,
			FragmentStatusWarning,
			FragmentStatusFailed,
		},
		status,
	)
}

var (
	_ jobFragmentReader         = (*repositoryFacade)(nil)
	_ downloadableChapterReader = (*memoryStore)(nil)
	_ downloadableChapterReader = (*PostgresStore)(nil)
	_ editedFragmentPreparer    = (*repositoryFacade)(nil)
	_ completedFragmentEditor   = (*memoryStore)(nil)
	_ completedFragmentEditor   = (*PostgresStore)(nil)
	_ jobFragmentReader         = (*memoryStore)(nil)
	_ jobFragmentReader         = (*PostgresStore)(nil)
	_ editedFragmentPreparer    = (*memoryStore)(nil)
	_ editedFragmentPreparer    = (*PostgresStore)(nil)
)
