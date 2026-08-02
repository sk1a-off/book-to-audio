package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgresStore) jobFragments(
	ctx context.Context,
	jobID string,
) (jobFragmentSnapshot, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return jobFragmentSnapshot{}, false, fmt.Errorf(
			"begin fragment catalog transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return jobFragmentSnapshot{}, false, nil
	}
	if err != nil {
		return jobFragmentSnapshot{}, false, fmt.Errorf("query fragment job: %w", err)
	}

	rows, err := tx.Query(
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

	result := make([]FragmentViewResource, 0, job.FragmentsCount)
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
			rows.Close()
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
	rows.Close()
	if err := rows.Err(); err != nil {
		return jobFragmentSnapshot{}, false, fmt.Errorf(
			"iterate job fragments: %w",
			err,
		)
	}
	if len(result) != job.FragmentsCount {
		return jobFragmentSnapshot{}, false, errors.New(
			"fragment catalog count does not match job",
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return jobFragmentSnapshot{}, false, fmt.Errorf(
			"commit fragment catalog transaction: %w",
			err,
		)
	}
	return jobFragmentSnapshot{Job: job, Fragments: result}, true, nil
}

func (s *PostgresStore) editAndPrepareFragment(
	ctx context.Context,
	id, text, revisionID string,
	now time.Time,
) (FragmentResource, jobTask, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"begin edit fragment transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	jobID, err := fragmentJobID(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	if err != nil {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"query fragment job: %w",
			err,
		)
	}
	job, err := scanJob(
		tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, jobID),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: job not found",
			errNotFound,
		)
	}
	if err != nil {
		return FragmentResource{}, jobTask{}, fmt.Errorf("lock fragment job: %w", err)
	}
	if job.Status == JobStatusPaused || job.Status == JobStatusCanceled {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: paused or canceled jobs cannot queue fragment edits",
			errConflict,
		)
	}

	var activeRewrite bool
	if err := tx.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1
			FROM rewrite_tasks
			WHERE job_id = $1 AND status IN ('queued', 'running')
		)`,
		jobID,
	).Scan(&activeRewrite); err != nil {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"check active text rewrite: %w",
			err,
		)
	}
	if activeRewrite {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: job has an active text rewrite",
			errConflict,
		)
	}

	resource, err := scanFragment(
		tx.QueryRow(ctx, fragmentSelect+` WHERE id = $1 FOR UPDATE`, id),
	)
	if err != nil {
		return FragmentResource{}, jobTask{}, fmt.Errorf("lock fragment: %w", err)
	}
	if !isEditableFragmentStatus(resource.Status) {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"%w: only ready, warning or failed fragments can be edited",
			errConflict,
		)
	}
	if revisionID == "" {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
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
		return FragmentResource{}, jobTask{}, fmt.Errorf(
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
		return FragmentResource{}, jobTask{}, err
	}

	resource.Text = text
	resource.STTText = ""
	resource.Status = FragmentStatusPending
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
		return FragmentResource{}, jobTask{}, mapPostgresWriteError(
			"update and queue edited fragment",
			err,
		)
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return FragmentResource{}, jobTask{}, err
	}
	if job.Status != JobStatusQueued && job.Status != JobStatusRunning {
		_, err = tx.Exec(
			ctx,
			`UPDATE jobs SET status = $2, updated_at = $3 WHERE id = $1`,
			jobID,
			JobStatusQueued,
			now,
		)
		if err != nil {
			return FragmentResource{}, jobTask{}, fmt.Errorf(
				"mark edited fragment job queued: %w",
				err,
			)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return FragmentResource{}, jobTask{}, fmt.Errorf(
			"commit edit fragment transaction: %w",
			err,
		)
	}

	return resource, jobTask{
		JobID:       jobID,
		FragmentIDs: []string{id},
		IsRetry:     true,
		PreviousStatuses: map[string]FragmentStatus{
			id: FragmentStatusWarning,
		},
		Settings: job.GenerationSettings,
	}, nil
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
			rows.Close()
			return archiveSnapshot{}, fmt.Errorf("scan chapter fragment: %w", err)
		}
		if len(fragment.AudioPCM) == 0 {
			rows.Close()
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
	rows.Close()
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

var _ fragmentWorkflowStore = (*PostgresStore)(nil)
