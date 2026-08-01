package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func jobStatusAllowsDeletion(status JobStatus) bool {
	switch status {
	case JobStatusCompleted, JobStatusCompletedWithWarnings, JobStatusFailed:
		return true
	default:
		return false
	}
}

func rewriteStatusIsActive(status RewriteTaskStatus) bool {
	return status == RewriteTaskStatusQueued || status == RewriteTaskStatusRunning
}

func (s *memoryStore) deleteJobForUser(
	_ context.Context,
	id string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.jobs[id]
	if !ok {
		return false, nil
	}
	if !jobStatusAllowsDeletion(record.Resource.Status) {
		return false, fmt.Errorf("%w: generation job is active", errConflict)
	}
	for _, rewrite := range s.rewriteTasks {
		if rewrite.Resource.JobID == id && rewriteStatusIsActive(rewrite.Resource.Status) {
			return false, fmt.Errorf("%w: job has an active LLM rewrite", errConflict)
		}
	}

	for rewriteID, rewrite := range s.rewriteTasks {
		if rewrite.Resource.JobID == id {
			delete(s.rewriteTasks, rewriteID)
		}
	}
	for _, fragmentID := range record.FragmentIDs {
		fragment := s.fragments[fragmentID]
		if fragment != nil {
			for _, revision := range fragment.TextRevisions {
				delete(s.textRevisionIDs, revision.ID)
			}
			for _, review := range fragment.ManualReviews {
				delete(s.manualReviewIDs, review.ID)
			}
		}
		delete(s.fragments, fragmentID)
	}
	delete(s.jobs, id)
	s.restoreLatestMemoryJobLocked(record.Resource.BookID, id)
	return true, nil
}

func (s *memoryStore) restoreLatestMemoryJobLocked(bookID, deletedID string) {
	bookEntry := s.books[bookID]
	if bookEntry == nil || bookEntry.LatestJobID != deletedID {
		return
	}
	bookEntry.LatestJobID = ""
	var latest *jobRecord
	for _, candidate := range s.jobs {
		if candidate.Resource.BookID != bookID {
			continue
		}
		if latest == nil ||
			candidate.Resource.CreatedAt.After(latest.Resource.CreatedAt) ||
			(candidate.Resource.CreatedAt.Equal(latest.Resource.CreatedAt) &&
				candidate.Resource.ID > latest.Resource.ID) {
			latest = candidate
		}
	}
	if latest != nil {
		bookEntry.LatestJobID = latest.Resource.ID
	}
}

func (s *memoryStore) deleteVoice(
	_ context.Context,
	id string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.voicesMap[id]; !ok {
		return false, nil
	}
	for _, job := range s.jobs {
		if job.Resource.VoiceID == id {
			return false, fmt.Errorf("%w: voice is referenced by a job", errConflict)
		}
	}
	delete(s.voicesMap, id)
	return true, nil
}

func (s *PostgresStore) deleteJobForUser(
	ctx context.Context,
	id string,
) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin delete job transaction: %w", err)
	}
	defer rollback(tx)

	var status JobStatus
	err = tx.QueryRow(
		ctx,
		`SELECT status FROM jobs WHERE id = $1 FOR UPDATE`,
		id,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock job for deletion: %w", err)
	}
	if !jobStatusAllowsDeletion(status) {
		return false, fmt.Errorf("%w: generation job is active", errConflict)
	}

	var activeRewrite bool
	if err := tx.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM rewrite_tasks
			WHERE job_id = $1 AND status IN ('queued', 'running')
		)`,
		id,
	).Scan(&activeRewrite); err != nil {
		return false, fmt.Errorf("check active LLM rewrite: %w", err)
	}
	if activeRewrite {
		return false, fmt.Errorf("%w: job has an active LLM rewrite", errConflict)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM rewrite_tasks WHERE job_id = $1`, id); err != nil {
		return false, fmt.Errorf("delete job rewrite history: %w", err)
	}
	command, err := tx.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("delete job: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, errors.New("locked job disappeared during deletion")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit delete job transaction: %w", err)
	}
	return true, nil
}

func (s *PostgresStore) deleteVoice(
	ctx context.Context,
	id string,
) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin delete voice transaction: %w", err)
	}
	defer rollback(tx)

	var lockedID string
	err = tx.QueryRow(
		ctx,
		`SELECT id FROM voices WHERE id = $1 FOR UPDATE`,
		id,
	).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock voice for deletion: %w", err)
	}

	var referenced bool
	if err := tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE voice_id = $1)`,
		id,
	).Scan(&referenced); err != nil {
		return false, fmt.Errorf("check voice references: %w", err)
	}
	if referenced {
		return false, fmt.Errorf("%w: voice is referenced by a job", errConflict)
	}

	command, err := tx.Exec(ctx, `DELETE FROM voices WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("delete voice: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, errors.New("locked voice disappeared during deletion")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit delete voice transaction: %w", err)
	}
	return true, nil
}
