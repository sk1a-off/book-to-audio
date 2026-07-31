package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *memoryStore) fragmentAudio(
	_ context.Context,
	fragmentID string,
) (fragmentAudioSnapshot, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.fragments[fragmentID]
	if !ok {
		return fragmentAudioSnapshot{}, false, nil
	}
	return fragmentAudioSnapshot{
		PCM:         slices.Clone(record.AudioPCM),
		SampleRate:  record.SampleRate,
		Channels:    record.Channels,
		SampleWidth: record.SampleWidth,
		DurationMS:  record.DurationMS,
	}, true, nil
}

func (s *memoryStore) approveFragment(
	_ context.Context,
	fragmentID, reviewID, reason string,
	now time.Time,
) (fragmentApproval, error) {
	if err := validateManualReviewInput(reviewID, reason); err != nil {
		return fragmentApproval{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.fragments[fragmentID]
	if !ok {
		return fragmentApproval{}, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	if record.Resource.Status != FragmentStatusWarning {
		return fragmentApproval{}, fmt.Errorf(
			"%w: only warning fragments can be approved",
			errConflict,
		)
	}
	if _, err := encodeFragmentAudioWAV(fragmentAudioSnapshot{
		PCM:         record.AudioPCM,
		SampleRate:  record.SampleRate,
		Channels:    record.Channels,
		SampleWidth: record.SampleWidth,
		DurationMS:  record.DurationMS,
	}); err != nil {
		return fragmentApproval{}, fmt.Errorf(
			"%w: fragment has no playable audio",
			errConflict,
		)
	}
	if s.manualReviewIDs == nil {
		s.manualReviewIDs = make(map[string]string)
	}
	if _, exists := s.manualReviewIDs[reviewID]; exists {
		return fragmentApproval{}, fmt.Errorf(
			"%w: duplicate manual review id",
			errConflict,
		)
	}
	job := s.jobs[record.Resource.JobID]
	if job == nil {
		return fragmentApproval{}, errors.New(
			"fragment references a missing job",
		)
	}

	review := FragmentManualReviewResource{
		ID:          reviewID,
		FragmentID:  fragmentID,
		Attempt:     record.Resource.Attempt,
		Decision:    FragmentManualReviewDecisionApproved,
		WarningCode: record.Resource.WarningCode,
		STTText:     record.Resource.STTText,
		Reason:      reason,
		CreatedAt:   now,
	}
	record.ManualReviews = append(record.ManualReviews, review)
	s.manualReviewIDs[reviewID] = fragmentID
	record.Resource.Status = FragmentStatusReady
	record.Resource.WarningCode = ""
	record.Resource.Error = ""
	record.Resource.UpdatedAt = now
	recomputeJob(job, s.fragments, now)

	return fragmentApproval{
		Fragment: record.Resource,
		Job:      job.Resource,
		Review:   review,
	}, nil
}

func (s *memoryStore) fragmentManualReviews(
	_ context.Context,
	fragmentID string,
) ([]FragmentManualReviewResource, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.fragments[fragmentID]
	if !ok {
		return nil, false, nil
	}
	reviews := slices.Clone(record.ManualReviews)
	if reviews == nil {
		reviews = make([]FragmentManualReviewResource, 0)
	}
	sort.Slice(reviews, func(left, right int) bool {
		if reviews[left].CreatedAt.Equal(reviews[right].CreatedAt) {
			return reviews[left].ID < reviews[right].ID
		}
		return reviews[left].CreatedAt.Before(reviews[right].CreatedAt)
	})
	return reviews, true, nil
}

func (s *PostgresStore) fragmentAudio(
	ctx context.Context,
	fragmentID string,
) (fragmentAudioSnapshot, bool, error) {
	var audio fragmentAudioSnapshot
	err := s.pool.QueryRow(
		ctx,
		`SELECT
			audio_pcm, sample_rate, channels, sample_width, duration_ms
		 FROM job_fragments
		 WHERE id = $1`,
		fragmentID,
	).Scan(
		&audio.PCM,
		&audio.SampleRate,
		&audio.Channels,
		&audio.SampleWidth,
		&audio.DurationMS,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fragmentAudioSnapshot{}, false, nil
	}
	if err != nil {
		return fragmentAudioSnapshot{}, false, fmt.Errorf(
			"query fragment audio: %w",
			err,
		)
	}
	audio.PCM = slices.Clone(audio.PCM)
	return audio, true, nil
}

func (s *PostgresStore) approveFragment(
	ctx context.Context,
	fragmentID, reviewID, reason string,
	now time.Time,
) (fragmentApproval, error) {
	if err := validateManualReviewInput(reviewID, reason); err != nil {
		return fragmentApproval{}, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fragmentApproval{}, fmt.Errorf(
			"begin approve fragment transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	jobID, err := fragmentJobID(ctx, tx, fragmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fragmentApproval{}, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	if err != nil {
		return fragmentApproval{}, fmt.Errorf(
			"query fragment job: %w",
			err,
		)
	}
	if err := lockJob(ctx, tx, jobID); err != nil {
		return fragmentApproval{}, err
	}

	resource, audio, metadata, err := lockFragmentSnapshot(
		ctx,
		tx,
		fragmentID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fragmentApproval{}, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	if err != nil {
		return fragmentApproval{}, fmt.Errorf("lock fragment: %w", err)
	}
	if resource.Status != FragmentStatusWarning {
		return fragmentApproval{}, fmt.Errorf(
			"%w: only warning fragments can be approved",
			errConflict,
		)
	}
	if _, err := encodeFragmentAudioWAV(fragmentAudioSnapshot{
		PCM:         audio,
		SampleRate:  metadata.sampleRate,
		Channels:    metadata.channels,
		SampleWidth: metadata.sampleWidth,
		DurationMS:  metadata.durationMS,
	}); err != nil {
		return fragmentApproval{}, fmt.Errorf(
			"%w: fragment has no playable audio",
			errConflict,
		)
	}

	review := FragmentManualReviewResource{
		ID:          reviewID,
		FragmentID:  fragmentID,
		Attempt:     resource.Attempt,
		Decision:    FragmentManualReviewDecisionApproved,
		WarningCode: resource.WarningCode,
		STTText:     resource.STTText,
		Reason:      reason,
		CreatedAt:   now,
	}
	_, err = tx.Exec(
		ctx,
		`INSERT INTO fragment_manual_reviews (
			id, fragment_id, attempt, decision, warning_code, stt_text,
			reason, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		review.ID,
		review.FragmentID,
		review.Attempt,
		review.Decision,
		review.WarningCode,
		review.STTText,
		review.Reason,
		review.CreatedAt,
	)
	if err != nil {
		return fragmentApproval{}, mapPostgresWriteError(
			"insert fragment manual review",
			err,
		)
	}

	resource.Status = FragmentStatusReady
	resource.WarningCode = ""
	resource.Error = ""
	resource.UpdatedAt = now
	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments
		 SET status = $2, warning_code = '', error_message = '',
		     updated_at = $3
		 WHERE id = $1`,
		fragmentID,
		resource.Status,
		resource.UpdatedAt,
	)
	if err != nil {
		return fragmentApproval{}, mapPostgresWriteError(
			"approve fragment",
			err,
		)
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return fragmentApproval{}, err
	}
	job, err := scanJob(
		tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, jobID),
	)
	if err != nil {
		return fragmentApproval{}, fmt.Errorf(
			"query approved fragment job: %w",
			err,
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return fragmentApproval{}, fmt.Errorf(
			"commit approve fragment transaction: %w",
			err,
		)
	}
	return fragmentApproval{
		Fragment: resource,
		Job:      job,
		Review:   review,
	}, nil
}

func (s *PostgresStore) fragmentManualReviews(
	ctx context.Context,
	fragmentID string,
) ([]FragmentManualReviewResource, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, false, fmt.Errorf(
			"begin fragment reviews transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	var storedID string
	err = tx.QueryRow(
		ctx,
		`SELECT id FROM job_fragments WHERE id = $1`,
		fragmentID,
	).Scan(&storedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf(
			"query reviewed fragment: %w",
			err,
		)
	}

	rows, err := tx.Query(
		ctx,
		`SELECT
			id, fragment_id, attempt, decision, warning_code, stt_text,
			reason, created_at
		 FROM fragment_manual_reviews
		 WHERE fragment_id = $1
		 ORDER BY created_at ASC, id ASC`,
		fragmentID,
	)
	if err != nil {
		return nil, false, fmt.Errorf(
			"query fragment manual reviews: %w",
			err,
		)
	}

	reviews := make([]FragmentManualReviewResource, 0)
	for rows.Next() {
		var review FragmentManualReviewResource
		if err := rows.Scan(
			&review.ID,
			&review.FragmentID,
			&review.Attempt,
			&review.Decision,
			&review.WarningCode,
			&review.STTText,
			&review.Reason,
			&review.CreatedAt,
		); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf(
				"scan fragment manual review: %w",
				err,
			)
		}
		reviews = append(reviews, review)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf(
			"iterate fragment manual reviews: %w",
			err,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf(
			"commit fragment reviews transaction: %w",
			err,
		)
	}
	return reviews, true, nil
}

func validateManualReviewInput(reviewID, reason string) error {
	if reviewID == "" {
		return fmt.Errorf("%w: manual review id is empty", errInvalid)
	}
	if len([]rune(reason)) > maxManualReviewReasonLength {
		return fmt.Errorf("%w: manual review reason is too long", errInvalid)
	}
	return nil
}
