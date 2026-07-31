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
	if fragment.Resource.Status != FragmentStatusWarning {
		return fmt.Errorf("%w: fragment is not in warning state", errConflict)
	}
	fragment.Resource.Status = FragmentStatusPending
	fragment.Resource.UpdatedAt = now
	// Keep the currently selected audio and transcript. The next candidate is
	// compared with them and only replaces them when Whisper scores it higher.
	recomputeJob(s.jobs[fragment.Resource.JobID], s.fragments, now)
	return nil
}

func (s *PostgresStore) prepareAutomaticWarningRetry(
	ctx context.Context,
	fragmentID string,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin automatic retry transaction: %w", err)
	}
	defer rollback(tx)
	jobID, err := fragmentJobID(ctx, tx, fragmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return fmt.Errorf("query automatic retry job: %w", err)
	}
	if err := lockJob(ctx, tx, jobID); err != nil {
		return err
	}
	resource, err := scanFragment(
		tx.QueryRow(ctx, fragmentSelect+` WHERE id = $1 FOR UPDATE`, fragmentID),
	)
	if err != nil {
		return fmt.Errorf("lock automatic retry fragment: %w", err)
	}
	if resource.Status != FragmentStatusWarning {
		return fmt.Errorf("%w: fragment is not in warning state", errConflict)
	}
	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments SET status = $2, updated_at = $3 WHERE id = $1`,
		fragmentID, FragmentStatusPending, now,
	)
	if err != nil {
		return mapPostgresWriteError("prepare automatic warning retry", err)
	}
	// Audio, transcript and metadata intentionally stay in the active row. The
	// next candidate is compared against them before it can replace the best one.
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit automatic retry transaction: %w", err)
	}
	return nil
}

func automaticRetryWarningCode(code string) bool {
	// Technical audio notes are shown to the reviewer but do not automatically
	// multiply expensive TTS jobs. Only a strong transcript mismatch gets one
	// automatic second candidate.
	return code == "transcript_mismatch"
}
