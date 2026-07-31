package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *memoryStore) prepareAutomaticWarningRetry(
	_ context.Context,
	fragmentID string,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fragment, ok := s.fragments[fragmentID]
	if !ok {
		return fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if fragment.Resource.Status != FragmentStatusWarning ||
		!automaticRetryWarningCode(fragment.Resource.WarningCode) {
		return fmt.Errorf(
			"%w: fragment does not have an automatically retryable warning",
			errConflict,
		)
	}

	fragment.Resource.Status = FragmentStatusPending
	fragment.Resource.WarningCode = ""
	fragment.Resource.Error = ""
	fragment.Resource.UpdatedAt = now
	job := s.jobs[fragment.Resource.JobID]
	recomputeJob(job, s.fragments, now)
	job.Resource.Status = JobStatusRunning

	return nil
}

func (s *PostgresStore) prepareAutomaticWarningRetry(
	ctx context.Context,
	fragmentID string,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf(
			"begin automatic warning retry transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	jobID, err := fragmentJobID(ctx, tx, fragmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return fmt.Errorf("query fragment job: %w", err)
	}
	if err := lockJob(ctx, tx, jobID); err != nil {
		return err
	}

	resource, err := scanFragment(
		tx.QueryRow(
			ctx,
			fragmentSelect+` WHERE id = $1 FOR UPDATE`,
			fragmentID,
		),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return fmt.Errorf("lock fragment: %w", err)
	}
	if resource.Status != FragmentStatusWarning ||
		!automaticRetryWarningCode(resource.WarningCode) {
		return fmt.Errorf(
			"%w: fragment does not have an automatically retryable warning",
			errConflict,
		)
	}

	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments
		 SET status = $2, warning_code = '', error_message = '', updated_at = $3
		 WHERE id = $1`,
		fragmentID,
		FragmentStatusPending,
		now,
	)
	if err != nil {
		return mapPostgresWriteError(
			"prepare automatic warning retry",
			err,
		)
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return err
	}
	_, err = tx.Exec(
		ctx,
		`UPDATE jobs SET status = $2, updated_at = $3 WHERE id = $1`,
		jobID,
		JobStatusRunning,
		now,
	)
	if err != nil {
		return mapPostgresWriteError(
			"keep automatic retry job running",
			err,
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf(
			"commit automatic warning retry transaction: %w",
			err,
		)
	}
	return nil
}

func automaticRetryWarningCode(code string) bool {
	return code == "transcript_mismatch" || code == "audio_warning"
}
