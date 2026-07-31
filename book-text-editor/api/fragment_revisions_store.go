package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *memoryStore) fragmentRevisions(
	_ context.Context,
	fragmentID string,
) (fragmentRevisionHistory, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.fragments[fragmentID]
	if !ok {
		return fragmentRevisionHistory{}, false, nil
	}
	current, ok := currentTextRevision(record.TextRevisions)
	if !ok {
		return fragmentRevisionHistory{}, false, errors.New(
			"fragment has no text revisions",
		)
	}

	return fragmentRevisionHistory{
		CurrentRevisionID: current.ID,
		Revisions:         cloneTextRevisions(record.TextRevisions),
	}, true, nil
}

func (s *memoryStore) restoreFragmentRevision(
	_ context.Context,
	fragmentID, selectedRevisionID, newRevisionID, reason string,
	now time.Time,
) (fragmentRevisionMutation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.fragments[fragmentID]
	if !ok {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	if record.Resource.Status == FragmentStatusGenerating {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: fragment is currently being processed",
			errConflict,
		)
	}
	if newRevisionID == "" {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: revision id is empty",
			errInvalid,
		)
	}
	if _, exists := s.textRevisionIDs[newRevisionID]; exists {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: duplicate revision id",
			errConflict,
		)
	}

	current, ok := currentTextRevision(record.TextRevisions)
	if !ok {
		return fragmentRevisionMutation{}, errors.New(
			"fragment has no current text revision",
		)
	}
	selected, ok := findTextRevision(
		record.TextRevisions,
		selectedRevisionID,
	)
	if !ok {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: revision not found",
			errRevisionNotFound,
		)
	}

	previousJobStatus := s.jobs[record.Resource.JobID].Resource.Status
	status, warningCode, err := restoredFragmentStatus(record.Resource.Status)
	if err != nil {
		return fragmentRevisionMutation{}, err
	}
	revision := newFragmentTextRevision(
		newRevisionID,
		fragmentID,
		current.RevisionNumber+1,
		FragmentTextRevisionSourceRestore,
		selected.Text,
		selected.ID,
		reason,
		now,
	)

	record.Resource.Text = selected.Text
	record.Resource.STTText = ""
	record.Resource.Status = status
	record.Resource.WarningCode = warningCode
	record.Resource.Error = ""
	record.Resource.UpdatedAt = now
	clearMemoryFragmentAudio(record)
	record.TextRevisions = append(record.TextRevisions, revision)
	s.textRevisionIDs[newRevisionID] = fragmentID

	job := s.jobs[record.Resource.JobID]
	recomputeJob(job, s.fragments, now)
	if previousJobStatus == JobStatusQueued &&
		status == FragmentStatusPending {
		job.Resource.Status = JobStatusQueued
	}

	return fragmentRevisionMutation{
		Fragment: record.Resource,
		Revision: cloneTextRevision(revision),
	}, nil
}

func (s *PostgresStore) fragmentRevisions(
	ctx context.Context,
	fragmentID string,
) (fragmentRevisionHistory, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return fragmentRevisionHistory{}, false, fmt.Errorf(
			"begin fragment revisions transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	var exists bool
	err = tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM job_fragments WHERE id = $1)`,
		fragmentID,
	).Scan(&exists)
	if err != nil {
		return fragmentRevisionHistory{}, false, fmt.Errorf(
			"check fragment: %w",
			err,
		)
	}
	if !exists {
		return fragmentRevisionHistory{}, false, nil
	}

	rows, err := tx.Query(
		ctx,
		textRevisionSelect+`
		 WHERE fragment_id = $1
		 ORDER BY revision_number ASC, id ASC`,
		fragmentID,
	)
	if err != nil {
		return fragmentRevisionHistory{}, false, fmt.Errorf(
			"query fragment revisions: %w",
			err,
		)
	}

	revisions := make([]FragmentTextRevisionResource, 0)
	for rows.Next() {
		revision, scanErr := scanTextRevision(rows)
		if scanErr != nil {
			rows.Close()
			return fragmentRevisionHistory{}, false, fmt.Errorf(
				"scan fragment revision: %w",
				scanErr,
			)
		}
		revisions = append(revisions, revision)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fragmentRevisionHistory{}, false, fmt.Errorf(
			"iterate fragment revisions: %w",
			err,
		)
	}
	if len(revisions) == 0 {
		return fragmentRevisionHistory{}, false, errors.New(
			"fragment has no text revisions",
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return fragmentRevisionHistory{}, false, fmt.Errorf(
			"commit fragment revisions transaction: %w",
			err,
		)
	}

	return fragmentRevisionHistory{
		CurrentRevisionID: revisions[len(revisions)-1].ID,
		Revisions:         revisions,
	}, true, nil
}

func (s *PostgresStore) restoreFragmentRevision(
	ctx context.Context,
	fragmentID, selectedRevisionID, newRevisionID, reason string,
	now time.Time,
) (fragmentRevisionMutation, error) {
	if newRevisionID == "" {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: revision id is empty",
			errInvalid,
		)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"begin restore fragment revision transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	jobID, err := fragmentJobID(ctx, tx, fragmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	if err != nil {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"query fragment job: %w",
			err,
		)
	}
	if err := lockJob(ctx, tx, jobID); err != nil {
		return fragmentRevisionMutation{}, err
	}

	resource, err := scanFragment(
		tx.QueryRow(
			ctx,
			fragmentSelect+` WHERE id = $1 FOR UPDATE`,
			fragmentID,
		),
	)
	if err != nil {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"lock fragment: %w",
			err,
		)
	}
	if resource.Status == FragmentStatusGenerating {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: fragment is currently being processed",
			errConflict,
		)
	}

	selected, err := scanTextRevision(
		tx.QueryRow(
			ctx,
			textRevisionSelect+`
			 WHERE fragment_id = $1 AND id = $2`,
			fragmentID,
			selectedRevisionID,
		),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"%w: revision not found",
			errRevisionNotFound,
		)
	}
	if err != nil {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"query selected fragment revision: %w",
			err,
		)
	}

	current, err := scanTextRevision(
		tx.QueryRow(
			ctx,
			textRevisionSelect+`
			 WHERE fragment_id = $1
			 ORDER BY revision_number DESC, id DESC
			 LIMIT 1`,
			fragmentID,
		),
	)
	if err != nil {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"query current fragment revision: %w",
			err,
		)
	}

	status, warningCode, err := restoredFragmentStatus(resource.Status)
	if err != nil {
		return fragmentRevisionMutation{}, err
	}
	var previousJobStatus JobStatus
	if err := tx.QueryRow(
		ctx,
		`SELECT status FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&previousJobStatus); err != nil {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"query locked job status: %w",
			err,
		)
	}

	revision := newFragmentTextRevision(
		newRevisionID,
		fragmentID,
		current.RevisionNumber+1,
		FragmentTextRevisionSourceRestore,
		selected.Text,
		selected.ID,
		reason,
		now,
	)
	if err := insertTextRevision(ctx, tx, revision); err != nil {
		return fragmentRevisionMutation{}, err
	}

	resource.Text = selected.Text
	resource.STTText = ""
	resource.Status = status
	resource.WarningCode = warningCode
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
		fragmentID,
		resource.Text,
		resource.Status,
		resource.WarningCode,
		resource.UpdatedAt,
	)
	if err != nil {
		return fragmentRevisionMutation{}, mapPostgresWriteError(
			"restore fragment text",
			err,
		)
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return fragmentRevisionMutation{}, err
	}
	if previousJobStatus == JobStatusQueued &&
		status == FragmentStatusPending {
		_, err = tx.Exec(
			ctx,
			`UPDATE jobs SET status = $2, updated_at = $3 WHERE id = $1`,
			jobID,
			JobStatusQueued,
			now,
		)
		if err != nil {
			return fragmentRevisionMutation{}, mapPostgresWriteError(
				"preserve queued job status",
				err,
			)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fragmentRevisionMutation{}, fmt.Errorf(
			"commit restore fragment revision transaction: %w",
			err,
		)
	}

	return fragmentRevisionMutation{
		Fragment: resource,
		Revision: revision,
	}, nil
}

func insertTextRevision(
	ctx context.Context,
	tx pgx.Tx,
	revision FragmentTextRevisionResource,
) error {
	_, err := tx.Exec(
		ctx,
		`INSERT INTO fragment_text_revisions (
			id, fragment_id, revision_number, source, text,
			parent_revision_id, model_id, model_revision, prompt, reason,
			created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		revision.ID,
		revision.FragmentID,
		revision.RevisionNumber,
		revision.Source,
		revision.Text,
		revision.ParentRevisionID,
		revision.ModelID,
		revision.ModelRevision,
		revision.Prompt,
		revision.Reason,
		revision.CreatedAt,
	)
	if err != nil {
		return mapPostgresWriteError("insert fragment text revision", err)
	}
	return nil
}

func scanTextRevision(row rowScanner) (FragmentTextRevisionResource, error) {
	var (
		revision      FragmentTextRevisionResource
		parent        pgtype.Text
		model         pgtype.Text
		modelRevision pgtype.Text
		prompt        pgtype.Text
		reason        pgtype.Text
	)
	err := row.Scan(
		&revision.ID,
		&revision.FragmentID,
		&revision.RevisionNumber,
		&revision.Source,
		&revision.Text,
		&parent,
		&model,
		&modelRevision,
		&prompt,
		&reason,
		&revision.CreatedAt,
	)
	revision.ParentRevisionID = pgTextPointer(parent)
	revision.ModelID = pgTextPointer(model)
	revision.ModelRevision = pgTextPointer(modelRevision)
	revision.Prompt = pgTextPointer(prompt)
	revision.Reason = pgTextPointer(reason)
	return revision, err
}

func newFragmentTextRevision(
	id, fragmentID string,
	number int,
	source FragmentTextRevisionSource,
	text, parentRevisionID, reason string,
	createdAt time.Time,
) FragmentTextRevisionResource {
	return FragmentTextRevisionResource{
		ID:               id,
		FragmentID:       fragmentID,
		RevisionNumber:   number,
		Source:           source,
		Text:             text,
		ParentRevisionID: optionalStringPointer(parentRevisionID),
		Reason:           optionalStringPointer(reason),
		CreatedAt:        createdAt,
	}
}

func currentTextRevision(
	revisions []FragmentTextRevisionResource,
) (FragmentTextRevisionResource, bool) {
	if len(revisions) == 0 {
		return FragmentTextRevisionResource{}, false
	}
	return revisions[len(revisions)-1], true
}

func findTextRevision(
	revisions []FragmentTextRevisionResource,
	id string,
) (FragmentTextRevisionResource, bool) {
	for _, revision := range revisions {
		if revision.ID == id {
			return revision, true
		}
	}
	return FragmentTextRevisionResource{}, false
}

func cloneTextRevisions(
	revisions []FragmentTextRevisionResource,
) []FragmentTextRevisionResource {
	result := slices.Clone(revisions)
	if result == nil {
		return make([]FragmentTextRevisionResource, 0)
	}
	for index := range result {
		result[index] = cloneTextRevision(result[index])
	}
	return result
}

func cloneTextRevision(
	revision FragmentTextRevisionResource,
) FragmentTextRevisionResource {
	revision.ParentRevisionID = cloneStringPointer(revision.ParentRevisionID)
	revision.ModelID = cloneStringPointer(revision.ModelID)
	revision.ModelRevision = cloneStringPointer(revision.ModelRevision)
	revision.Prompt = cloneStringPointer(revision.Prompt)
	revision.Reason = cloneStringPointer(revision.Reason)
	return revision
}

func restoredFragmentStatus(
	current FragmentStatus,
) (FragmentStatus, string, error) {
	switch current {
	case FragmentStatusPending:
		return FragmentStatusPending, "", nil
	case FragmentStatusReady,
		FragmentStatusWarning,
		FragmentStatusFailed:
		return FragmentStatusWarning, "text_restored", nil
	case FragmentStatusGenerating:
		return "", "", fmt.Errorf(
			"%w: fragment is currently being processed",
			errConflict,
		)
	default:
		return "", "", fmt.Errorf(
			"%w: unsupported fragment status %q",
			errInvalid,
			current,
		)
	}
}

func clearMemoryFragmentAudio(record *fragmentRecord) {
	record.AudioPCM = nil
	record.SampleRate = 0
	record.Channels = 0
	record.SampleWidth = 0
	record.DurationMS = 0
	record.STTLanguage = ""
	record.WorkerNotes = nil
}

func optionalStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func pgTextPointer(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

const textRevisionSelect = `SELECT
	id, fragment_id, revision_number, source, text, parent_revision_id,
	model_id, model_revision, prompt, reason, created_at
FROM fragment_text_revisions`
