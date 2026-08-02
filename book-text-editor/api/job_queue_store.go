package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

type generationQueueSnapshot struct {
	Jobs      []QueueJobResource
	Fragments []QueueFragmentResource
}

func generationQueueStatus(status JobStatus) bool {
	switch status {
	case JobStatusRunning, JobStatusQueued, JobStatusPaused:
		return true
	default:
		return false
	}
}

func generationQueueStatusRank(status JobStatus) int {
	switch status {
	case JobStatusRunning:
		return 0
	case JobStatusQueued:
		return 1
	case JobStatusPaused:
		return 2
	default:
		return 3
	}
}

func (s *memoryStore) generationQueue(
	_ context.Context,
) (generationQueueSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := make([]*jobRecord, 0, len(s.jobs))
	for _, record := range s.jobs {
		if generationQueueStatus(record.Resource.Status) {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(left, right int) bool {
		leftRank := generationQueueStatusRank(records[left].Resource.Status)
		rightRank := generationQueueStatusRank(records[right].Resource.Status)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if records[left].Resource.CreatedAt.Equal(records[right].Resource.CreatedAt) {
			return records[left].Resource.ID < records[right].Resource.ID
		}
		return records[left].Resource.CreatedAt.Before(records[right].Resource.CreatedAt)
	})

	snapshot := generationQueueSnapshot{
		Jobs:      make([]QueueJobResource, 0, len(records)),
		Fragments: make([]QueueFragmentResource, 0),
	}
	fragmentPosition := 0
	for jobIndex, record := range records {
		bookTitle := ""
		if bookEntry := s.books[record.Resource.BookID]; bookEntry != nil {
			bookTitle = bookEntry.Resource.Title
		}
		snapshot.Jobs = append(snapshot.Jobs, QueueJobResource{
			Job:           record.Resource,
			BookTitle:     bookTitle,
			QueuePosition: jobIndex + 1,
		})

		for _, fragmentID := range record.FragmentIDs {
			fragment := s.fragments[fragmentID]
			if fragment == nil ||
				(fragment.Resource.Status != FragmentStatusPending &&
					fragment.Resource.Status != FragmentStatusGenerating) {
				continue
			}
			fragmentPosition++
			snapshot.Fragments = append(snapshot.Fragments, QueueFragmentResource{
				Fragment:      fragment.Resource,
				BookID:        record.Resource.BookID,
				BookTitle:     bookTitle,
				ChapterTitle:  fragment.ChapterTitle,
				JobStatus:     record.Resource.Status,
				QueuePosition: fragmentPosition,
			})
		}
	}
	return snapshot, nil
}

func (s *memoryStore) recoverGenerationQueue(
	_ context.Context,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, job := range s.jobs {
		if job.Resource.Status != JobStatusRunning &&
			job.Resource.Status != JobStatusQueued &&
			job.Resource.Status != JobStatusFailed {
			continue
		}
		changed := job.Resource.Status == JobStatusRunning
		for _, fragmentID := range job.FragmentIDs {
			fragment := s.fragments[fragmentID]
			if fragment == nil {
				continue
			}
			interruptedFailure :=
				(job.Resource.Status == JobStatusRunning ||
					job.Resource.Status == JobStatusFailed) &&
					fragment.Resource.Status == FragmentStatusFailed &&
					isInterruptedGenerationError(fragment.Resource.Error)
			orphanedGenerating :=
				(job.Resource.Status == JobStatusRunning ||
					job.Resource.Status == JobStatusQueued) &&
					fragment.Resource.Status == FragmentStatusGenerating
			if orphanedGenerating || interruptedFailure {
				fragment.Resource.Status = FragmentStatusPending
				fragment.Resource.Error = ""
				fragment.Resource.UpdatedAt = now
				changed = true
			}
		}
		if changed {
			recomputeJob(job, s.fragments, now)
			if job.Resource.FragmentsPending > 0 {
				job.Resource.Status = JobStatusQueued
			}
		}
	}
	return nil
}

func isInterruptedGenerationError(message string) bool {
	return message == "generation interrupted" || message == "validation interrupted"
}

func (s *memoryStore) claimGenerationTask(
	_ context.Context,
	now time.Time,
) (jobTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var selected *jobRecord
	for _, candidate := range s.jobs {
		if candidate.Resource.Status != JobStatusQueued {
			continue
		}
		if selected == nil ||
			candidate.Resource.CreatedAt.Before(selected.Resource.CreatedAt) ||
			(candidate.Resource.CreatedAt.Equal(selected.Resource.CreatedAt) &&
				candidate.Resource.ID < selected.Resource.ID) {
			selected = candidate
		}
	}
	if selected == nil {
		return jobTask{}, false, nil
	}

	fragmentIDs := make([]string, 0, len(selected.FragmentIDs))
	for _, fragmentID := range selected.FragmentIDs {
		fragment := s.fragments[fragmentID]
		if fragment != nil && fragment.Resource.Status == FragmentStatusPending {
			fragmentIDs = append(fragmentIDs, fragmentID)
		}
	}
	if len(fragmentIDs) == 0 {
		recomputeJob(selected, s.fragments, now)
		return jobTask{}, false, nil
	}

	selected.Resource.Status = JobStatusRunning
	selected.Resource.UpdatedAt = now
	return jobTask{
		JobID:       selected.Resource.ID,
		FragmentIDs: fragmentIDs,
		Claimed:     true,
		Settings:    selected.Resource.GenerationSettings,
	}, true, nil
}

func (s *memoryStore) releaseGenerationTask(
	_ context.Context,
	jobID, fragmentID string,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[jobID]
	if !ok {
		return fmt.Errorf("%w: job not found", errNotFound)
	}
	if fragmentID == "" {
		resetGeneratingMemoryFragments(job, s.fragments, now)
	} else if fragment := s.fragments[fragmentID]; fragment != nil &&
		fragment.Resource.JobID == jobID &&
		fragment.Resource.Status == FragmentStatusGenerating {
		fragment.Resource.Status = FragmentStatusPending
		fragment.Resource.Error = ""
		fragment.Resource.UpdatedAt = now
	}
	recomputeJob(job, s.fragments, now)
	if job.Resource.Status == JobStatusRunning && job.Resource.FragmentsPending > 0 {
		job.Resource.Status = JobStatusQueued
	}
	return nil
}

func (s *memoryStore) pauseJob(
	_ context.Context,
	jobID string,
	now time.Time,
) (JobResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[jobID]
	if !ok {
		return JobResource{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if job.Resource.Status != JobStatusQueued && job.Resource.Status != JobStatusRunning {
		return JobResource{}, fmt.Errorf("%w: job is not active", errConflict)
	}
	resetGeneratingMemoryFragments(job, s.fragments, now)
	job.Resource.Status = JobStatusPaused
	job.Resource.UpdatedAt = now
	return job.Resource, nil
}

func (s *memoryStore) resumeJob(
	_ context.Context,
	jobID string,
	now time.Time,
) (JobResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[jobID]
	if !ok {
		return JobResource{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if job.Resource.Status != JobStatusPaused {
		return JobResource{}, fmt.Errorf("%w: job is not paused", errConflict)
	}
	if job.Resource.FragmentsPending == 0 {
		return JobResource{}, fmt.Errorf("%w: job has no pending fragments", errConflict)
	}
	job.Resource.Status = JobStatusQueued
	job.Resource.UpdatedAt = now
	return job.Resource, nil
}

func (s *memoryStore) cancelJob(
	_ context.Context,
	jobID string,
	now time.Time,
) (JobResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[jobID]
	if !ok {
		return JobResource{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	switch job.Resource.Status {
	case JobStatusQueued, JobStatusRunning, JobStatusPaused:
	default:
		return JobResource{}, fmt.Errorf("%w: job is already terminal", errConflict)
	}
	resetGeneratingMemoryFragments(job, s.fragments, now)
	job.Resource.Status = JobStatusCanceled
	job.Resource.UpdatedAt = now
	return job.Resource, nil
}

func resetGeneratingMemoryFragments(
	job *jobRecord,
	fragments map[string]*fragmentRecord,
	now time.Time,
) {
	for _, fragmentID := range job.FragmentIDs {
		fragment := fragments[fragmentID]
		if fragment != nil && fragment.Resource.Status == FragmentStatusGenerating {
			fragment.Resource.Status = FragmentStatusPending
			fragment.Resource.Error = ""
			fragment.Resource.UpdatedAt = now
		}
	}
}

func (s *PostgresStore) generationQueue(
	ctx context.Context,
) (generationQueueSnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return generationQueueSnapshot{}, fmt.Errorf("begin queue transaction: %w", err)
	}
	defer rollback(tx)

	rows, err := tx.Query(ctx, jobSelect+`
		WHERE status IN ('running', 'queued', 'paused')
		ORDER BY CASE status
			WHEN 'running' THEN 0
			WHEN 'queued' THEN 1
			ELSE 2
		END, created_at ASC, id ASC`)
	if err != nil {
		return generationQueueSnapshot{}, fmt.Errorf("query generation queue jobs: %w", err)
	}
	jobs := make([]JobResource, 0)
	for rows.Next() {
		resource, scanErr := scanJob(rows)
		if scanErr != nil {
			rows.Close()
			return generationQueueSnapshot{}, fmt.Errorf("scan generation queue job: %w", scanErr)
		}
		jobs = append(jobs, resource)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return generationQueueSnapshot{}, fmt.Errorf("iterate generation queue jobs: %w", err)
	}

	bookTitles := make(map[string]string)
	result := generationQueueSnapshot{
		Jobs:      make([]QueueJobResource, 0, len(jobs)),
		Fragments: make([]QueueFragmentResource, 0),
	}
	for index, job := range jobs {
		bookTitle, ok := bookTitles[job.BookID]
		if !ok {
			if err := tx.QueryRow(
				ctx,
				`SELECT title FROM books WHERE id = $1`,
				job.BookID,
			).Scan(&bookTitle); err != nil {
				return generationQueueSnapshot{}, fmt.Errorf("query queued book title: %w", err)
			}
			bookTitles[job.BookID] = bookTitle
		}
		result.Jobs = append(result.Jobs, QueueJobResource{
			Job:           job,
			BookTitle:     bookTitle,
			QueuePosition: index + 1,
		})
	}

	fragmentRows, err := tx.Query(ctx, `SELECT
		f.id, f.job_id, f.chapter_number, f.ordinal, f.text, f.stt_text,
		f.status, f.warning_code, f.error_message, f.attempt, f.updated_at,
		j.book_id, b.title, f.chapter_title, j.status
		FROM job_fragments AS f
		JOIN jobs AS j ON j.id = f.job_id
		JOIN books AS b ON b.id = j.book_id
		WHERE j.status IN ('running', 'queued', 'paused')
		  AND f.status IN ('pending', 'generating')
		ORDER BY CASE j.status
			WHEN 'running' THEN 0
			WHEN 'queued' THEN 1
			ELSE 2
		END, j.created_at ASC, j.id ASC, f.ordinal ASC, f.id ASC`)
	if err != nil {
		return generationQueueSnapshot{}, fmt.Errorf("query generation queue fragments: %w", err)
	}
	position := 0
	for fragmentRows.Next() {
		position++
		var item QueueFragmentResource
		if err := fragmentRows.Scan(
			&item.Fragment.ID,
			&item.Fragment.JobID,
			&item.Fragment.ChapterNumber,
			&item.Fragment.Ordinal,
			&item.Fragment.Text,
			&item.Fragment.STTText,
			&item.Fragment.Status,
			&item.Fragment.WarningCode,
			&item.Fragment.Error,
			&item.Fragment.Attempt,
			&item.Fragment.UpdatedAt,
			&item.BookID,
			&item.BookTitle,
			&item.ChapterTitle,
			&item.JobStatus,
		); err != nil {
			fragmentRows.Close()
			return generationQueueSnapshot{}, fmt.Errorf("scan generation queue fragment: %w", err)
		}
		item.QueuePosition = position
		result.Fragments = append(result.Fragments, item)
	}
	fragmentRows.Close()
	if err := fragmentRows.Err(); err != nil {
		return generationQueueSnapshot{}, fmt.Errorf("iterate generation queue fragments: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return generationQueueSnapshot{}, fmt.Errorf("commit queue transaction: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) recoverGenerationQueue(
	ctx context.Context,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin queue recovery transaction: %w", err)
	}
	defer rollback(tx)

	affected := make(map[string]struct{})
	rows, err := tx.Query(ctx, `SELECT j.id FROM jobs AS j
		WHERE j.status = 'running'
		   OR (j.status = 'queued' AND EXISTS (
			SELECT 1 FROM job_fragments AS f
			WHERE f.job_id = j.id AND f.status = 'generating'
		   ))
		   OR (j.status = 'failed' AND EXISTS (
			SELECT 1 FROM job_fragments AS f
			WHERE f.job_id = j.id
			  AND f.status = 'failed'
			  AND f.error_message IN (
				'generation interrupted', 'validation interrupted'
			  )
		   ))
		ORDER BY j.id
		FOR UPDATE OF j`)
	if err != nil {
		return fmt.Errorf("lock interrupted jobs: %w", err)
	}
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return fmt.Errorf("scan interrupted job: %w", err)
		}
		affected[jobID] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate interrupted jobs: %w", err)
	}

	rows, err = tx.Query(ctx, `UPDATE job_fragments AS f
		SET status = 'pending', error_message = '', updated_at = $1
		FROM jobs AS j
		WHERE f.job_id = j.id
		  AND j.status IN ('running', 'queued')
		  AND f.status = 'generating'
		RETURNING f.job_id`, now)
	if err != nil {
		return mapPostgresWriteError("recover generating fragments", err)
	}
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return fmt.Errorf("scan recovered generating fragment: %w", err)
		}
		affected[jobID] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate recovered generating fragments: %w", err)
	}

	rows, err = tx.Query(ctx, `UPDATE job_fragments AS f
		SET status = 'pending', error_message = '', updated_at = $1
		FROM jobs AS j
		WHERE f.job_id = j.id
		  AND j.status IN ('running', 'failed')
		  AND f.status = 'failed'
		  AND f.error_message IN ('generation interrupted', 'validation interrupted')
		RETURNING f.job_id`, now)
	if err != nil {
		return mapPostgresWriteError("recover legacy interrupted fragments", err)
	}
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy interrupted fragment: %w", err)
		}
		affected[jobID] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy interrupted fragments: %w", err)
	}

	jobIDs := make([]string, 0, len(affected))
	for jobID := range affected {
		jobIDs = append(jobIDs, jobID)
	}
	sort.Strings(jobIDs)
	for _, jobID := range jobIDs {
		if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE jobs
			SET status = 'queued', updated_at = $2
			WHERE id = $1 AND status = 'running' AND fragments_pending > 0`,
			jobID, now)
		if err != nil {
			return mapPostgresWriteError("requeue interrupted job", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit queue recovery transaction: %w", err)
	}
	return nil
}

func (s *PostgresStore) claimGenerationTask(
	ctx context.Context,
	now time.Time,
) (jobTask, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return jobTask{}, false, fmt.Errorf("begin claim generation task: %w", err)
	}
	defer rollback(tx)

	resource, err := scanJob(tx.QueryRow(ctx, jobSelect+`
		WHERE status = 'queued'
		  AND EXISTS (
			SELECT 1 FROM job_fragments
			WHERE job_id = jobs.id AND status = 'pending'
		  )
		ORDER BY created_at ASC, id ASC
		LIMIT 1
		FOR UPDATE`))
	if errors.Is(err, pgx.ErrNoRows) {
		return jobTask{}, false, nil
	}
	if err != nil {
		return jobTask{}, false, fmt.Errorf("select generation task: %w", err)
	}

	rows, err := tx.Query(ctx, `SELECT id
		FROM job_fragments
		WHERE job_id = $1 AND status = 'pending'
		ORDER BY ordinal ASC, id ASC
		FOR UPDATE`, resource.ID)
	if err != nil {
		return jobTask{}, false, fmt.Errorf("query queued fragments: %w", err)
	}
	fragmentIDs := make([]string, 0, resource.FragmentsPending)
	for rows.Next() {
		var fragmentID string
		if err := rows.Scan(&fragmentID); err != nil {
			rows.Close()
			return jobTask{}, false, fmt.Errorf("scan queued fragment: %w", err)
		}
		fragmentIDs = append(fragmentIDs, fragmentID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return jobTask{}, false, fmt.Errorf("iterate queued fragments: %w", err)
	}

	_, err = tx.Exec(ctx, `UPDATE jobs
		SET status = 'running', updated_at = $2
		WHERE id = $1`, resource.ID, now)
	if err != nil {
		return jobTask{}, false, mapPostgresWriteError("claim generation task", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return jobTask{}, false, fmt.Errorf("commit claim generation task: %w", err)
	}
	return jobTask{
		JobID:       resource.ID,
		FragmentIDs: fragmentIDs,
		Claimed:     true,
		Settings:    resource.GenerationSettings,
	}, true, nil
}

func (s *PostgresStore) releaseGenerationTask(
	ctx context.Context,
	jobID, fragmentID string,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin release generation task: %w", err)
	}
	defer rollback(tx)

	if err := lockJob(ctx, tx, jobID); err != nil {
		return err
	}
	if fragmentID != "" {
		_, err = tx.Exec(ctx, `UPDATE job_fragments
			SET status = 'pending', error_message = '', updated_at = $3
			WHERE id = $1 AND job_id = $2 AND status = 'generating'`,
			fragmentID, jobID, now)
		if err != nil {
			return mapPostgresWriteError("release generating fragment", err)
		}
	} else {
		_, err = tx.Exec(ctx, `UPDATE job_fragments
			SET status = 'pending', error_message = '', updated_at = $2
			WHERE job_id = $1 AND status = 'generating'`, jobID, now)
		if err != nil {
			return mapPostgresWriteError("release generating fragments", err)
		}
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE jobs
		SET status = 'queued', updated_at = $2
		WHERE id = $1 AND status = 'running' AND fragments_pending > 0`, jobID, now)
	if err != nil {
		return mapPostgresWriteError("requeue interrupted job", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit release generation task: %w", err)
	}
	return nil
}

func (s *PostgresStore) pauseJob(
	ctx context.Context,
	jobID string,
	now time.Time,
) (JobResource, error) {
	return s.transitionActiveJob(ctx, jobID, JobStatusPaused, now)
}

func (s *PostgresStore) cancelJob(
	ctx context.Context,
	jobID string,
	now time.Time,
) (JobResource, error) {
	return s.transitionActiveJob(ctx, jobID, JobStatusCanceled, now)
}

func (s *PostgresStore) transitionActiveJob(
	ctx context.Context,
	jobID string,
	target JobStatus,
	now time.Time,
) (JobResource, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JobResource{}, fmt.Errorf("begin job transition: %w", err)
	}
	defer rollback(tx)

	resource, err := scanJob(
		tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, jobID),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobResource{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if err != nil {
		return JobResource{}, fmt.Errorf("lock job transition: %w", err)
	}
	allowed := resource.Status == JobStatusQueued || resource.Status == JobStatusRunning
	if target == JobStatusCanceled {
		allowed = allowed || resource.Status == JobStatusPaused
	}
	if !allowed {
		return JobResource{}, fmt.Errorf("%w: job cannot transition to %s", errConflict, target)
	}

	_, err = tx.Exec(ctx, `UPDATE job_fragments
		SET status = 'pending', error_message = '', updated_at = $2
		WHERE job_id = $1 AND status = 'generating'`, jobID, now)
	if err != nil {
		return JobResource{}, mapPostgresWriteError("stop generating fragments", err)
	}
	_, err = tx.Exec(ctx, `UPDATE jobs
		SET status = $2, updated_at = $3
		WHERE id = $1`, jobID, target, now)
	if err != nil {
		return JobResource{}, mapPostgresWriteError("transition generation job", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobResource{}, fmt.Errorf("commit job transition: %w", err)
	}
	resource.Status = target
	resource.UpdatedAt = now
	return resource, nil
}

func (s *PostgresStore) resumeJob(
	ctx context.Context,
	jobID string,
	now time.Time,
) (JobResource, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JobResource{}, fmt.Errorf("begin resume job: %w", err)
	}
	defer rollback(tx)

	resource, err := scanJob(
		tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, jobID),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobResource{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if err != nil {
		return JobResource{}, fmt.Errorf("lock job for resume: %w", err)
	}
	if resource.Status != JobStatusPaused {
		return JobResource{}, fmt.Errorf("%w: job is not paused", errConflict)
	}
	if resource.FragmentsPending == 0 {
		return JobResource{}, fmt.Errorf("%w: job has no pending fragments", errConflict)
	}

	_, err = tx.Exec(ctx, `UPDATE jobs
		SET status = 'queued', updated_at = $2
		WHERE id = $1`, jobID, now)
	if err != nil {
		return JobResource{}, mapPostgresWriteError("resume generation job", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobResource{}, fmt.Errorf("commit resume job: %w", err)
	}
	resource.Status = JobStatusQueued
	resource.UpdatedAt = now
	return resource, nil
}
