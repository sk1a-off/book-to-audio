package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"book-text-editor/internal/book"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore persists the temporary API model in normalized PostgreSQL
// tables. It deliberately does not run migrations during application startup.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// OpenPostgresStore creates and verifies a PostgreSQL connection pool.
func OpenPostgresStore(
	ctx context.Context,
	databaseURL string,
) (*PostgresStore, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("database URL is required")
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL configuration: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return &PostgresStore{pool: pool}, nil
}

// Close releases all pool resources.
func (s *PostgresStore) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func (s *PostgresStore) close() {
	s.Close()
}

func (s *PostgresStore) createBook(
	ctx context.Context,
	id string,
	parsed book.Book,
	now time.Time,
) (BookResource, error) {
	resource := BookResource{
		ID:             id,
		Title:          parsed.Title,
		Authors:        nonNilStrings(parsed.Authors),
		Format:         "fb2",
		ChaptersCount:  len(parsed.Chapters),
		FragmentsCount: parsed.SegmentCount(),
		Status:         "ready",
		CreatedAt:      now,
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return BookResource{}, fmt.Errorf("begin create book transaction: %w", err)
	}
	defer rollback(tx)

	_, err = tx.Exec(
		ctx,
		`INSERT INTO books (
			id, title, authors, format, chapters_count, fragments_count,
			status, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		resource.ID,
		resource.Title,
		resource.Authors,
		resource.Format,
		resource.ChaptersCount,
		resource.FragmentsCount,
		resource.Status,
		resource.CreatedAt,
	)
	if err != nil {
		return BookResource{}, mapPostgresWriteError("insert book", err)
	}

	globalOrdinal := 0
	for chapterIndex, chapter := range parsed.Chapters {
		_, err = tx.Exec(
			ctx,
			`INSERT INTO chapters (book_id, number, ordinal, title)
			 VALUES ($1, $2, $3, $4)`,
			id,
			chapter.Number,
			chapterIndex+1,
			chapter.Title,
		)
		if err != nil {
			return BookResource{}, mapPostgresWriteError("insert chapter", err)
		}

		for fragmentIndex, text := range chapter.Segments {
			globalOrdinal++
			_, err = tx.Exec(
				ctx,
				`INSERT INTO book_fragments (
					book_id, ordinal, chapter_number, ordinal_in_chapter, text
				) VALUES ($1, $2, $3, $4, $5)`,
				id,
				globalOrdinal,
				chapter.Number,
				fragmentIndex+1,
				text,
			)
			if err != nil {
				return BookResource{}, mapPostgresWriteError(
					"insert book fragment",
					err,
				)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return BookResource{}, fmt.Errorf("commit create book transaction: %w", err)
	}

	return cloneBookResource(resource), nil
}

func (s *PostgresStore) book(
	ctx context.Context,
	id string,
) (BookResource, bool, error) {
	resource, err := scanBook(s.pool.QueryRow(ctx, bookSelect+` WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BookResource{}, false, nil
	}
	if err != nil {
		return BookResource{}, false, fmt.Errorf("query book: %w", err)
	}

	return resource, true, nil
}

func (s *PostgresStore) createVoice(
	ctx context.Context,
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

	_, err := s.pool.Exec(
		ctx,
		`INSERT INTO voices (
			id, name, mode, format, content_type, size_bytes,
			has_reference_text, status, reference_text, reference_audio,
			created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		resource.ID,
		resource.Name,
		resource.Mode,
		resource.Format,
		resource.ContentType,
		resource.SizeBytes,
		resource.HasReferenceText,
		resource.Status,
		referenceText,
		nonNilBytes(audio),
		resource.CreatedAt,
	)
	if err != nil {
		return VoiceResource{}, mapPostgresWriteError("insert voice", err)
	}

	return resource, nil
}

func (s *PostgresStore) voice(
	ctx context.Context,
	id string,
) (VoiceResource, bool, error) {
	resource, err := scanVoice(
		s.pool.QueryRow(ctx, voiceSelect+` WHERE id = $1`, id),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return VoiceResource{}, false, nil
	}
	if err != nil {
		return VoiceResource{}, false, fmt.Errorf("query voice: %w", err)
	}

	return resource, true, nil
}

func (s *PostgresStore) voices(
	ctx context.Context,
) ([]VoiceResource, error) {
	rows, err := s.pool.Query(
		ctx,
		voiceSelect+` ORDER BY created_at ASC, id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query voices: %w", err)
	}
	defer rows.Close()

	result := make([]VoiceResource, 0)
	for rows.Next() {
		resource, scanErr := scanVoice(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan voice: %w", scanErr)
		}
		result = append(result, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate voices: %w", err)
	}

	return result, nil
}

func (s *PostgresStore) createJob(
	ctx context.Context,
	id, bookID, voiceID string,
	fragmentIDs, originalRevisionIDs []string,
	settings GenerationSettings,
	now time.Time,
) (JobResource, jobTask, error) {
	if err := validateGenerationSettings(settings); err != nil {
		return JobResource{}, jobTask{}, fmt.Errorf("%w: %v", errInvalid, err)
	}
	if duplicate := firstDuplicate(fragmentIDs); duplicate != "" {
		return JobResource{}, jobTask{}, fmt.Errorf(
			"%w: duplicate fragment id %q",
			errInvalid,
			duplicate,
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
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JobResource{}, jobTask{}, fmt.Errorf(
			"begin create job transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	var expectedFragments int
	err = tx.QueryRow(
		ctx,
		`SELECT fragments_count FROM books WHERE id = $1 FOR SHARE`,
		bookID,
	).Scan(&expectedFragments)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobResource{}, jobTask{}, fmt.Errorf("%w: book not found", errNotFound)
	}
	if err != nil {
		return JobResource{}, jobTask{}, fmt.Errorf("lock book: %w", err)
	}

	var voiceExists bool
	err = tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM voices WHERE id = $1)`,
		voiceID,
	).Scan(&voiceExists)
	if err != nil {
		return JobResource{}, jobTask{}, fmt.Errorf("check voice: %w", err)
	}
	if !voiceExists {
		return JobResource{}, jobTask{}, fmt.Errorf("%w: voice not found", errNotFound)
	}
	if len(fragmentIDs) != expectedFragments {
		return JobResource{}, jobTask{}, fmt.Errorf(
			"%w: got %d fragment ids for %d book fragments",
			errInvalid,
			len(fragmentIDs),
			expectedFragments,
		)
	}

	sourceFragments, err := loadBookFragments(ctx, tx, bookID)
	if err != nil {
		return JobResource{}, jobTask{}, err
	}
	if len(sourceFragments) != expectedFragments {
		return JobResource{}, jobTask{}, errors.New(
			"book fragment count does not match stored fragments",
		)
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
	_, err = tx.Exec(
		ctx,
		`INSERT INTO jobs (
			id, book_id, voice_id, status, fragments_count,
			fragments_pending, fragments_ready, fragments_warnings,
			fragments_failed, omnivoice_num_steps,
			omnivoice_guidance_scale, omnivoice_speed,
			omnivoice_normalize_text, omnivoice_denoise,
			omnivoice_t_shift, omnivoice_layer_penalty_factor,
			omnivoice_position_temperature, omnivoice_class_temperature,
			omnivoice_preprocess_prompt, omnivoice_postprocess_output,
			omnivoice_audio_chunk_duration,
			omnivoice_audio_chunk_threshold, omnivoice_pad_duration,
			omnivoice_fade_duration, whisper_beam_size,
			whisper_patience, whisper_temperature, whisper_vad_filter,
			whisper_word_timestamps, automatic_warning_retries,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			$13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23,
			$24, $25, $26, $27, $28, $29, $30, $31, $32
		)`,
		resource.ID,
		resource.BookID,
		resource.VoiceID,
		resource.Status,
		resource.FragmentsCount,
		resource.FragmentsPending,
		resource.FragmentsReady,
		resource.FragmentsWarnings,
		resource.FragmentsFailed,
		resource.GenerationSettings.OmniVoice.NumSteps,
		resource.GenerationSettings.OmniVoice.GuidanceScale,
		resource.GenerationSettings.OmniVoice.Speed,
		resource.GenerationSettings.OmniVoice.NormalizeText,
		resource.GenerationSettings.OmniVoice.Denoise,
		resource.GenerationSettings.OmniVoice.TShift,
		resource.GenerationSettings.OmniVoice.LayerPenaltyFactor,
		resource.GenerationSettings.OmniVoice.PositionTemperature,
		resource.GenerationSettings.OmniVoice.ClassTemperature,
		resource.GenerationSettings.OmniVoice.PreprocessPrompt,
		resource.GenerationSettings.OmniVoice.PostprocessOutput,
		resource.GenerationSettings.OmniVoice.AudioChunkDuration,
		resource.GenerationSettings.OmniVoice.AudioChunkThreshold,
		resource.GenerationSettings.OmniVoice.PadDuration,
		resource.GenerationSettings.OmniVoice.FadeDuration,
		resource.GenerationSettings.Whisper.BeamSize,
		resource.GenerationSettings.Whisper.Patience,
		resource.GenerationSettings.Whisper.Temperature,
		resource.GenerationSettings.Whisper.VADFilter,
		resource.GenerationSettings.Whisper.WordTimestamps,
		resource.GenerationSettings.AutomaticWarningRetries,
		resource.CreatedAt,
		resource.UpdatedAt,
	)
	if err != nil {
		return JobResource{}, jobTask{}, mapPostgresWriteError("insert job", err)
	}

	for index, source := range sourceFragments {
		_, err = tx.Exec(
			ctx,
			`INSERT INTO job_fragments (
				id, job_id, book_id, source_ordinal, chapter_number,
				chapter_title, ordinal, text, status, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			fragmentIDs[index],
			id,
			bookID,
			source.ordinal,
			source.chapterNumber,
			source.chapterTitle,
			source.ordinal,
			source.text,
			FragmentStatusPending,
			now,
		)
		if err != nil {
			return JobResource{}, jobTask{}, mapPostgresWriteError(
				"insert job fragment",
				err,
			)
		}
		if err := insertTextRevision(
			ctx,
			tx,
			newFragmentTextRevision(
				originalRevisionIDs[index],
				fragmentIDs[index],
				1,
				FragmentTextRevisionSourceOriginal,
				source.text,
				"",
				"",
				now,
			),
		); err != nil {
			return JobResource{}, jobTask{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return JobResource{}, jobTask{}, fmt.Errorf(
			"commit create job transaction: %w",
			err,
		)
	}

	return resource, jobTask{
		JobID:       id,
		FragmentIDs: slices.Clone(fragmentIDs),
		Settings:    settings,
	}, nil
}

func (s *PostgresStore) deleteJob(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	return nil
}

func (s *PostgresStore) job(
	ctx context.Context,
	id string,
) (JobResource, bool, error) {
	resource, err := scanJob(s.pool.QueryRow(ctx, jobSelect+` WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return JobResource{}, false, nil
	}
	if err != nil {
		return JobResource{}, false, fmt.Errorf("query job: %w", err)
	}

	return resource, true, nil
}

func (s *PostgresStore) listJobs(
	ctx context.Context,
	filter jobListFilter,
) (jobListPage, error) {
	if err := filter.validate(); err != nil {
		return jobListPage{}, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return jobListPage{}, fmt.Errorf("begin jobs transaction: %w", err)
	}
	defer rollback(tx)

	statuses := make([]string, len(filter.Statuses))
	for index, status := range filter.Statuses {
		statuses[index] = string(status)
	}

	var total int64
	if len(statuses) == 0 {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&total)
	} else {
		err = tx.QueryRow(
			ctx,
			`SELECT count(*) FROM jobs WHERE status = ANY($1::text[])`,
			statuses,
		).Scan(&total)
	}
	if err != nil {
		return jobListPage{}, fmt.Errorf("count jobs: %w", err)
	}

	var rows pgx.Rows
	if len(statuses) == 0 {
		rows, err = tx.Query(
			ctx,
			jobSelect+`
			 ORDER BY created_at DESC, id DESC
			 LIMIT $1 OFFSET $2`,
			filter.Limit,
			filter.Offset,
		)
	} else {
		rows, err = tx.Query(
			ctx,
			jobSelect+`
			 WHERE status = ANY($1::text[])
			 ORDER BY created_at DESC, id DESC
			 LIMIT $2 OFFSET $3`,
			statuses,
			filter.Limit,
			filter.Offset,
		)
	}
	if err != nil {
		return jobListPage{}, fmt.Errorf("query jobs: %w", err)
	}

	result := make([]JobResource, 0)
	for rows.Next() {
		resource, scanErr := scanJob(rows)
		if scanErr != nil {
			rows.Close()
			return jobListPage{}, fmt.Errorf("scan job list item: %w", scanErr)
		}
		result = append(result, resource)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return jobListPage{}, fmt.Errorf("iterate jobs: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return jobListPage{}, fmt.Errorf("commit jobs transaction: %w", err)
	}

	return jobListPage{Jobs: result, Total: total}, nil
}

func (s *PostgresStore) jobIssues(
	ctx context.Context,
	jobID string,
) ([]FragmentResource, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, false, fmt.Errorf("begin job issues transaction: %w", err)
	}
	defer rollback(tx)

	var exists bool
	err = tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`,
		jobID,
	).Scan(&exists)
	if err != nil {
		return nil, false, fmt.Errorf("check job: %w", err)
	}
	if !exists {
		return nil, false, nil
	}

	rows, err := tx.Query(
		ctx,
		fragmentSelect+`
		 WHERE job_id = $1 AND status IN ('warning', 'failed')
		 ORDER BY ordinal ASC, id ASC`,
		jobID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("query job issues: %w", err)
	}

	result := make([]FragmentResource, 0)
	for rows.Next() {
		resource, scanErr := scanFragment(rows)
		if scanErr != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan job issue: %w", scanErr)
		}
		result = append(result, resource)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate job issues: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit job issues transaction: %w", err)
	}

	return result, true, nil
}

func (s *PostgresStore) editFragment(
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
	if resource.Status == FragmentStatusGenerating {
		return FragmentResource{}, fmt.Errorf(
			"%w: fragment is currently being processed",
			errConflict,
		)
	}
	if resource.Status != FragmentStatusWarning &&
		resource.Status != FragmentStatusFailed {
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

func (s *PostgresStore) prepareRetry(
	ctx context.Context,
	jobID string,
	requested []string,
	now time.Time,
) (jobTask, error) {
	if duplicate := firstDuplicate(requested); duplicate != "" {
		return jobTask{}, fmt.Errorf(
			"%w: duplicate fragment id %q",
			errInvalid,
			duplicate,
		)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return jobTask{}, fmt.Errorf("begin prepare retry transaction: %w", err)
	}
	defer rollback(tx)

	jobResource, err := scanJob(
		tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, jobID),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobTask{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if err != nil {
		return jobTask{}, fmt.Errorf("lock job: %w", err)
	}
	if jobResource.Status == JobStatusQueued ||
		jobResource.Status == JobStatusRunning {
		return jobTask{}, fmt.Errorf("%w: job is already active", errConflict)
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
		return jobTask{}, fmt.Errorf("check active text rewrite: %w", err)
	}
	if activeRewrite {
		return jobTask{}, fmt.Errorf(
			"%w: job has an active text rewrite",
			errConflict,
		)
	}

	selected, previous, err := retryCandidates(ctx, tx, jobID, requested)
	if err != nil {
		return jobTask{}, err
	}
	if len(selected) == 0 {
		return jobTask{}, fmt.Errorf(
			"%w: job has no retryable fragments",
			errConflict,
		)
	}

	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments
		 SET status = $2, updated_at = $3
		 WHERE id = ANY($1::TEXT[])`,
		selected,
		FragmentStatusPending,
		now,
	)
	if err != nil {
		return jobTask{}, mapPostgresWriteError("queue retry fragments", err)
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return jobTask{}, err
	}
	_, err = tx.Exec(
		ctx,
		`UPDATE jobs SET status = $2, updated_at = $3 WHERE id = $1`,
		jobID,
		JobStatusQueued,
		now,
	)
	if err != nil {
		return jobTask{}, fmt.Errorf("mark retry job queued: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return jobTask{}, fmt.Errorf("commit prepare retry transaction: %w", err)
	}

	return jobTask{
		JobID:            jobID,
		FragmentIDs:      selected,
		IsRetry:          true,
		PreviousStatuses: previous,
		Settings:         jobResource.GenerationSettings,
	}, nil
}

func (s *PostgresStore) cancelRetry(
	ctx context.Context,
	task jobTask,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin cancel retry transaction: %w", err)
	}
	defer rollback(tx)

	if err := lockJob(ctx, tx, task.JobID); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return err
	}

	ids := make([]string, 0, len(task.PreviousStatuses))
	for id := range task.PreviousStatuses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		_, err = tx.Exec(
			ctx,
			`UPDATE job_fragments
			 SET status = $3, updated_at = $4
			 WHERE id = $1 AND job_id = $2 AND status = 'pending'`,
			id,
			task.JobID,
			task.PreviousStatuses[id],
			now,
		)
		if err != nil {
			return mapPostgresWriteError("restore retry fragment", err)
		}
	}
	if err := recomputePostgresJob(ctx, tx, task.JobID, now); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cancel retry transaction: %w", err)
	}
	return nil
}

func (s *PostgresStore) startTask(
	ctx context.Context,
	task jobTask,
	now time.Time,
) error {
	tag, err := s.pool.Exec(
		ctx,
		`UPDATE jobs SET status = $2, updated_at = $3 WHERE id = $1`,
		task.JobID,
		JobStatusRunning,
		now,
	)
	if err != nil {
		return mapPostgresWriteError("start job task", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job not found", errNotFound)
	}
	return nil
}

func (s *PostgresStore) workItem(
	ctx context.Context,
	fragmentID string,
) (fragmentWorkItem, error) {
	row := s.pool.QueryRow(
		ctx,
		`SELECT
			f.id, f.job_id, f.chapter_number, f.ordinal, f.text,
			f.stt_text, f.status, f.warning_code, f.error_message,
			f.attempt, f.updated_at,
			v.id, v.name, v.mode, v.format, v.content_type,
			v.size_bytes, v.has_reference_text, v.status, v.created_at,
			v.reference_text, v.reference_audio
		 FROM job_fragments AS f
		 JOIN jobs AS j ON j.id = f.job_id
		 JOIN voices AS v ON v.id = j.voice_id
		 WHERE f.id = $1`,
		fragmentID,
	)

	var result fragmentWorkItem
	err := row.Scan(
		&result.Resource.ID,
		&result.Resource.JobID,
		&result.Resource.ChapterNumber,
		&result.Resource.Ordinal,
		&result.Resource.Text,
		&result.Resource.STTText,
		&result.Resource.Status,
		&result.Resource.WarningCode,
		&result.Resource.Error,
		&result.Resource.Attempt,
		&result.Resource.UpdatedAt,
		&result.Voice.Resource.ID,
		&result.Voice.Resource.Name,
		&result.Voice.Resource.Mode,
		&result.Voice.Resource.Format,
		&result.Voice.Resource.ContentType,
		&result.Voice.Resource.SizeBytes,
		&result.Voice.Resource.HasReferenceText,
		&result.Voice.Resource.Status,
		&result.Voice.Resource.CreatedAt,
		&result.Voice.ReferenceText,
		&result.Voice.Audio,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fragmentWorkItem{}, fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return fragmentWorkItem{}, fmt.Errorf("query fragment work item: %w", err)
	}
	result.Voice.Audio = slices.Clone(result.Voice.Audio)

	return result, nil
}

func (s *PostgresStore) startFragment(
	ctx context.Context,
	fragmentID string,
	now time.Time,
) (FragmentResource, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return FragmentResource{}, fmt.Errorf(
			"begin start fragment transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	resource, err := scanFragment(
		tx.QueryRow(ctx, fragmentSelect+` WHERE id = $1 FOR UPDATE`, fragmentID),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return FragmentResource{}, fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return FragmentResource{}, fmt.Errorf("lock fragment: %w", err)
	}
	if resource.Status != FragmentStatusPending {
		return FragmentResource{}, fmt.Errorf(
			"%w: fragment is not pending",
			errConflict,
		)
	}

	resource.Status = FragmentStatusGenerating
	resource.Attempt++
	resource.UpdatedAt = now
	resource.WarningCode = ""
	resource.Error = ""
	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments
		 SET status = $2, attempt = $3, updated_at = $4,
		     warning_code = '', error_message = ''
		 WHERE id = $1`,
		fragmentID,
		resource.Status,
		resource.Attempt,
		resource.UpdatedAt,
	)
	if err != nil {
		return FragmentResource{}, mapPostgresWriteError("start fragment", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return FragmentResource{}, fmt.Errorf(
			"commit start fragment transaction: %w",
			err,
		)
	}
	return resource, nil
}

func (s *PostgresStore) completeFragment(
	ctx context.Context,
	fragmentID string,
	result fragmentResult,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin complete fragment transaction: %w", err)
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
	resource, currentAudio, metadata, err := lockFragmentSnapshot(ctx, tx, fragmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return fmt.Errorf("lock fragment: %w", err)
	}
	if resource.Status != FragmentStatusGenerating {
		return fmt.Errorf("%w: fragment is not generating", errConflict)
	}

	candidate := cloneFragmentResult(result)
	candidate.WarningCode = effectiveFragmentWarning(resource.Text, candidate)
	candidateResource := resource
	candidateResource.STTText = candidate.STTText
	candidateResource.Status = fragmentStatusForResult(candidate)
	candidateResource.WarningCode = candidate.WarningCode
	candidateResource.Error = ""
	candidateResource.UpdatedAt = now
	if err := insertAttempt(
		ctx, tx, candidateResource, nonNilBytes(candidate.AudioPCM),
		candidate.SampleRate, candidate.Channels, candidate.SampleWidth,
		candidate.DurationMS, candidate.STTLanguage,
		nonNilStrings(candidate.WorkerNotes), now,
	); err != nil {
		return err
	}

	current := fragmentResultFromStored(
		resource, currentAudio, metadata.sampleRate, metadata.channels,
		metadata.sampleWidth, metadata.durationMS, metadata.sttLanguage,
		metadata.workerNotes,
	)
	selected, _ := chooseBestFragmentResult(resource.Text, current, candidate)
	selected.WarningCode = effectiveFragmentWarning(resource.Text, selected)
	resource.STTText = selected.STTText
	resource.Status = fragmentStatusForResult(selected)
	resource.WarningCode = selected.WarningCode
	resource.Error = ""
	resource.UpdatedAt = now
	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments
		 SET stt_text = $2, status = $3, warning_code = $4,
		     error_message = '', audio_pcm = $5, sample_rate = $6,
		     channels = $7, sample_width = $8, duration_ms = $9,
		     stt_language = $10, worker_notes = $11, updated_at = $12
		 WHERE id = $1`,
		fragmentID, resource.STTText, resource.Status, resource.WarningCode,
		nonNilBytes(selected.AudioPCM), selected.SampleRate, selected.Channels,
		selected.SampleWidth, selected.DurationMS, selected.STTLanguage,
		nonNilStrings(selected.WorkerNotes), resource.UpdatedAt,
	)
	if err != nil {
		return mapPostgresWriteError("complete fragment", err)
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit complete fragment transaction: %w", err)
	}
	return nil
}

func (s *PostgresStore) failFragment(
	ctx context.Context,
	fragmentID, message string,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin fail fragment transaction: %w", err)
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

	resource, audio, audioMetadata, err := lockFragmentSnapshot(
		ctx,
		tx,
		fragmentID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: fragment not found", errNotFound)
	}
	if err != nil {
		return fmt.Errorf("lock fragment: %w", err)
	}
	if resource.Status != FragmentStatusGenerating {
		return fmt.Errorf("%w: fragment is not generating", errConflict)
	}

	resource.Status = FragmentStatusFailed
	resource.WarningCode = ""
	resource.Error = message
	resource.UpdatedAt = now
	_, err = tx.Exec(
		ctx,
		`UPDATE job_fragments
		 SET status = $2, warning_code = '', error_message = $3,
		     updated_at = $4
		 WHERE id = $1`,
		fragmentID,
		resource.Status,
		resource.Error,
		resource.UpdatedAt,
	)
	if err != nil {
		return mapPostgresWriteError("fail fragment", err)
	}
	if err := insertAttempt(
		ctx,
		tx,
		resource,
		nonNilBytes(audio),
		audioMetadata.sampleRate,
		audioMetadata.channels,
		audioMetadata.sampleWidth,
		audioMetadata.durationMS,
		audioMetadata.sttLanguage,
		audioMetadata.workerNotes,
		now,
	); err != nil {
		return err
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fail fragment transaction: %w", err)
	}
	return nil
}

func (s *PostgresStore) finishTask(
	ctx context.Context,
	jobID string,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin finish task transaction: %w", err)
	}
	defer rollback(tx)

	if err := lockJob(ctx, tx, jobID); err != nil {
		return err
	}
	if err := recomputePostgresJob(ctx, tx, jobID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit finish task transaction: %w", err)
	}
	return nil
}

func (s *PostgresStore) latestJobID(
	ctx context.Context,
	bookID string,
) (string, bool, error) {
	var id string
	err := s.pool.QueryRow(
		ctx,
		`SELECT id
		 FROM jobs
		 WHERE book_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`,
		bookID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query latest job: %w", err)
	}
	return id, true, nil
}

func (s *PostgresStore) jobChapterCatalog(
	ctx context.Context,
	jobID string,
) (chapterCatalog, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return chapterCatalog{}, fmt.Errorf(
			"begin chapter catalog transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	jobResource, err := loadCompletedArchiveJob(ctx, tx, jobID)
	if err != nil {
		return chapterCatalog{}, err
	}

	rows, err := tx.Query(
		ctx,
		`SELECT
			chapter_number,
			MIN(chapter_title),
			COUNT(*)::BIGINT,
			COALESCE(SUM(duration_ms), 0)::BIGINT,
			COUNT(DISTINCT chapter_title)::BIGINT,
			BOOL_AND(COALESCE(octet_length(audio_pcm), 0) > 0)
		 FROM job_fragments
		 WHERE job_id = $1
		 GROUP BY chapter_number
		 ORDER BY chapter_number ASC`,
		jobID,
	)
	if err != nil {
		return chapterCatalog{}, fmt.Errorf(
			"query chapter catalog: %w",
			err,
		)
	}
	chapters := make([]chapterSummary, 0)
	var totalFragments int64
	for rows.Next() {
		var (
			summary        chapterSummary
			fragmentsCount int64
			distinctTitles int64
			allReady       bool
		)
		if err := rows.Scan(
			&summary.Number,
			&summary.Title,
			&fragmentsCount,
			&summary.DurationMS,
			&distinctTitles,
			&allReady,
		); err != nil {
			rows.Close()
			return chapterCatalog{}, fmt.Errorf(
				"scan chapter catalog: %w",
				err,
			)
		}
		if summary.Number <= 0 ||
			fragmentsCount <= 0 ||
			summary.DurationMS < 0 ||
			distinctTitles != 1 {
			rows.Close()
			return chapterCatalog{}, errors.New(
				"chapter catalog contains invalid metadata",
			)
		}
		if !allReady {
			rows.Close()
			return chapterCatalog{}, fmt.Errorf(
				"%w: chapter fragments are not ready",
				errConflict,
			)
		}
		summary.FragmentsCount = int(fragmentsCount)
		totalFragments += fragmentsCount
		chapters = append(chapters, summary)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return chapterCatalog{}, fmt.Errorf(
			"iterate chapter catalog: %w",
			err,
		)
	}
	if totalFragments != int64(jobResource.FragmentsCount) {
		return chapterCatalog{}, errors.New(
			"chapter catalog fragment count does not match job",
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return chapterCatalog{}, fmt.Errorf(
			"commit chapter catalog transaction: %w",
			err,
		)
	}

	return chapterCatalog{
		BookID:   jobResource.BookID,
		Chapters: chapters,
	}, nil
}

func (s *PostgresStore) archiveSnapshot(
	ctx context.Context,
	jobID string,
) (archiveSnapshot, error) {
	return s.loadArchiveSnapshot(ctx, jobID, 0)
}

func (s *PostgresStore) chapterArchiveSnapshot(
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

	return s.loadArchiveSnapshot(ctx, jobID, chapterNumber)
}

func (s *PostgresStore) loadArchiveSnapshot(
	ctx context.Context,
	jobID string,
	chapterNumber int,
) (archiveSnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf(
			"begin archive snapshot transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	jobResource, err := loadCompletedArchiveJob(ctx, tx, jobID)
	if err != nil {
		return archiveSnapshot{}, err
	}

	bookResource, err := scanBook(
		tx.QueryRow(ctx, bookSelect+` WHERE id = $1`, jobResource.BookID),
	)
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf("query archive book: %w", err)
	}
	voiceResource, err := scanVoice(
		tx.QueryRow(ctx, voiceSelect+` WHERE id = $1`, jobResource.VoiceID),
	)
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf("query archive voice: %w", err)
	}

	query := `SELECT
		id, job_id, chapter_number, ordinal, text, stt_text, status,
		warning_code, error_message, attempt, updated_at,
		chapter_title, audio_pcm, sample_rate, channels, sample_width,
		duration_ms, worker_notes
	 FROM job_fragments
	 WHERE job_id = $1`
	arguments := []any{jobID}
	fragmentCapacity := jobResource.FragmentsCount
	if chapterNumber > 0 {
		query += ` AND chapter_number = $2`
		arguments = append(arguments, chapterNumber)
		fragmentCapacity = 0
	}
	query += ` ORDER BY ordinal ASC, id ASC`

	rows, err := tx.Query(
		ctx,
		query,
		arguments...,
	)
	if err != nil {
		return archiveSnapshot{}, fmt.Errorf("query archive fragments: %w", err)
	}

	fragments := make([]archiveFragment, 0, fragmentCapacity)
	for rows.Next() {
		var fragment archiveFragment
		err = rows.Scan(
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
		)
		if err != nil {
			rows.Close()
			return archiveSnapshot{}, fmt.Errorf(
				"scan archive fragment: %w",
				err,
			)
		}
		if len(fragment.AudioPCM) == 0 {
			rows.Close()
			return archiveSnapshot{}, fmt.Errorf(
				"%w: fragment audio is not ready",
				errConflict,
			)
		}
		fragment.AudioPCM = slices.Clone(fragment.AudioPCM)
		fragment.WorkerNotes = nonNilStrings(fragment.WorkerNotes)
		fragments = append(fragments, fragment)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return archiveSnapshot{}, fmt.Errorf(
			"iterate archive fragments: %w",
			err,
		)
	}
	if chapterNumber > 0 && len(fragments) == 0 {
		return archiveSnapshot{}, fmt.Errorf(
			"%w: chapter %d has no generated fragments",
			errChapterNotFound,
			chapterNumber,
		)
	}
	if chapterNumber == 0 && len(fragments) != jobResource.FragmentsCount {
		return archiveSnapshot{}, errors.New(
			"archive fragment count does not match job",
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return archiveSnapshot{}, fmt.Errorf(
			"commit archive snapshot transaction: %w",
			err,
		)
	}

	return archiveSnapshot{
		Book:      bookResource,
		Voice:     voiceResource,
		Job:       jobResource,
		Fragments: fragments,
	}, nil
}

func loadCompletedArchiveJob(
	ctx context.Context,
	tx pgx.Tx,
	jobID string,
) (JobResource, error) {
	jobResource, err := scanJob(
		tx.QueryRow(ctx, jobSelect+` WHERE id = $1`, jobID),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobResource{}, fmt.Errorf("%w: job not found", errNotFound)
	}
	if err != nil {
		return JobResource{}, fmt.Errorf("query archive job: %w", err)
	}
	if jobResource.Status != JobStatusCompleted &&
		jobResource.Status != JobStatusCompletedWithWarnings {
		return JobResource{}, fmt.Errorf(
			"%w: generation must finish before full-book export",
			errConflict,
		)
	}
	// Warnings do not block export once generation is terminal and every
	// fragment has selected audio.
	return jobResource, nil
}

type sourceBookFragment struct {
	ordinal       int
	chapterNumber int
	chapterTitle  string
	text          string
}

func loadBookFragments(
	ctx context.Context,
	tx pgx.Tx,
	bookID string,
) ([]sourceBookFragment, error) {
	rows, err := tx.Query(
		ctx,
		`SELECT f.ordinal, f.chapter_number, c.title, f.text
		 FROM book_fragments AS f
		 JOIN chapters AS c
		   ON c.book_id = f.book_id AND c.number = f.chapter_number
		 WHERE f.book_id = $1
		 ORDER BY f.ordinal ASC`,
		bookID,
	)
	if err != nil {
		return nil, fmt.Errorf("query book fragments: %w", err)
	}
	defer rows.Close()

	result := make([]sourceBookFragment, 0)
	for rows.Next() {
		var fragment sourceBookFragment
		if err := rows.Scan(
			&fragment.ordinal,
			&fragment.chapterNumber,
			&fragment.chapterTitle,
			&fragment.text,
		); err != nil {
			return nil, fmt.Errorf("scan book fragment: %w", err)
		}
		result = append(result, fragment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate book fragments: %w", err)
	}
	return result, nil
}

func retryCandidates(
	ctx context.Context,
	tx pgx.Tx,
	jobID string,
	requested []string,
) ([]string, map[string]FragmentStatus, error) {
	if len(requested) == 0 {
		rows, err := tx.Query(
			ctx,
			`SELECT id, status
			 FROM job_fragments
			 WHERE job_id = $1 AND status IN ('warning', 'failed')
			 ORDER BY ordinal ASC, id ASC
			 FOR UPDATE`,
			jobID,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("query retryable fragments: %w", err)
		}
		defer rows.Close()

		selected := make([]string, 0)
		previous := make(map[string]FragmentStatus)
		for rows.Next() {
			var (
				id     string
				status FragmentStatus
			)
			if err := rows.Scan(&id, &status); err != nil {
				return nil, nil, fmt.Errorf("scan retryable fragment: %w", err)
			}
			selected = append(selected, id)
			previous[id] = status
		}
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("iterate retryable fragments: %w", err)
		}
		return selected, previous, nil
	}

	type candidate struct {
		jobID  string
		status FragmentStatus
	}
	candidates := make(map[string]candidate, len(requested))
	rows, err := tx.Query(
		ctx,
		`SELECT id, job_id, status
		 FROM job_fragments
		 WHERE id = ANY($1::TEXT[])`,
		requested,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("query requested retry fragments: %w", err)
	}
	for rows.Next() {
		var (
			id        string
			candidate candidate
		)
		if err := rows.Scan(&id, &candidate.jobID, &candidate.status); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf(
				"scan requested retry fragment: %w",
				err,
			)
		}
		candidates[id] = candidate
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf(
			"iterate requested retry fragments: %w",
			err,
		)
	}

	for _, id := range requested {
		candidate, exists := candidates[id]
		if !exists {
			return nil, nil, fmt.Errorf(
				"%w: fragment %q not found",
				errNotFound,
				id,
			)
		}
		if candidate.jobID != jobID {
			return nil, nil, fmt.Errorf(
				"%w: fragment %q does not belong to job",
				errInvalid,
				id,
			)
		}
	}

	lockOrder := slices.Clone(requested)
	sort.Strings(lockOrder)
	rows, err = tx.Query(
		ctx,
		`SELECT id, status
		 FROM job_fragments
		 WHERE job_id = $1 AND id = ANY($2::TEXT[])
		 ORDER BY id ASC
		 FOR UPDATE`,
		jobID,
		lockOrder,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("lock retry fragments: %w", err)
	}
	lockedStatuses := make(map[string]FragmentStatus, len(requested))
	for rows.Next() {
		var (
			id     string
			status FragmentStatus
		)
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan locked retry fragment: %w", err)
		}
		lockedStatuses[id] = status
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate locked retry fragments: %w", err)
	}

	previous := make(map[string]FragmentStatus, len(requested))
	for _, id := range requested {
		status, exists := lockedStatuses[id]
		if !exists {
			return nil, nil, fmt.Errorf(
				"%w: fragment %q not found",
				errNotFound,
				id,
			)
		}
		if status != FragmentStatusWarning && status != FragmentStatusFailed {
			return nil, nil, fmt.Errorf(
				"%w: fragment %q is not retryable",
				errInvalid,
				id,
			)
		}
		previous[id] = status
	}

	return slices.Clone(requested), previous, nil
}

func fragmentJobID(
	ctx context.Context,
	tx pgx.Tx,
	fragmentID string,
) (string, error) {
	var jobID string
	err := tx.QueryRow(
		ctx,
		`SELECT job_id FROM job_fragments WHERE id = $1`,
		fragmentID,
	).Scan(&jobID)
	return jobID, err
}

func lockJob(ctx context.Context, tx pgx.Tx, jobID string) error {
	var lockedID string
	err := tx.QueryRow(
		ctx,
		`SELECT id FROM jobs WHERE id = $1 FOR UPDATE`,
		jobID,
	).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: job not found", errNotFound)
	}
	if err != nil {
		return fmt.Errorf("lock job: %w", err)
	}
	return nil
}

func recomputePostgresJob(
	ctx context.Context,
	tx pgx.Tx,
	jobID string,
	now time.Time,
) error {
	tag, err := tx.Exec(
		ctx,
		`WITH counts AS (
			SELECT
				count(*) FILTER (
					WHERE status IN ('pending', 'generating')
				)::INTEGER AS pending,
				count(*) FILTER (WHERE status = 'ready')::INTEGER AS ready,
				count(*) FILTER (WHERE status = 'warning')::INTEGER AS warnings,
				count(*) FILTER (WHERE status = 'failed')::INTEGER AS failed,
				coalesce(bool_or(status = 'generating'), false) AS active
			FROM job_fragments
			WHERE job_id = $1
		)
		UPDATE jobs
		SET
			fragments_pending = counts.pending,
			fragments_ready = counts.ready,
			fragments_warnings = counts.warnings,
			fragments_failed = counts.failed,
			status = CASE
				WHEN counts.active OR counts.pending > 0 THEN 'running'
				WHEN counts.failed > 0 THEN 'failed'
				WHEN counts.warnings > 0 THEN 'completed_with_warnings'
				ELSE 'completed'
			END,
			updated_at = $2
		FROM counts
		WHERE jobs.id = $1`,
		jobID,
		now,
	)
	if err != nil {
		return mapPostgresWriteError("recompute job", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job not found", errNotFound)
	}
	return nil
}

type audioSnapshotMetadata struct {
	sampleRate  int
	channels    int
	sampleWidth int
	durationMS  int
	sttLanguage string
	workerNotes []string
}

func lockFragmentSnapshot(
	ctx context.Context,
	tx pgx.Tx,
	fragmentID string,
) (FragmentResource, []byte, audioSnapshotMetadata, error) {
	row := tx.QueryRow(
		ctx,
		`SELECT
			id, job_id, chapter_number, ordinal, text, stt_text, status,
			warning_code, error_message, attempt, updated_at,
			audio_pcm, sample_rate, channels, sample_width, duration_ms,
			stt_language, worker_notes
		 FROM job_fragments
		 WHERE id = $1
		 FOR UPDATE`,
		fragmentID,
	)
	var (
		resource FragmentResource
		audio    []byte
		metadata audioSnapshotMetadata
	)
	err := row.Scan(
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
		&audio,
		&metadata.sampleRate,
		&metadata.channels,
		&metadata.sampleWidth,
		&metadata.durationMS,
		&metadata.sttLanguage,
		&metadata.workerNotes,
	)
	return resource, audio, metadata, err
}

func insertAttempt(
	ctx context.Context,
	tx pgx.Tx,
	resource FragmentResource,
	audio []byte,
	sampleRate, channels, sampleWidth, durationMS int,
	sttLanguage string,
	workerNotes []string,
	completedAt time.Time,
) error {
	_, err := tx.Exec(
		ctx,
		`INSERT INTO fragment_attempts (
			fragment_id, attempt, text, stt_text, status, warning_code,
			error_message, audio_pcm, sample_rate, channels, sample_width,
			duration_ms, stt_language, worker_notes, completed_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			$13, $14, $15
		)`,
		resource.ID,
		resource.Attempt,
		resource.Text,
		resource.STTText,
		resource.Status,
		resource.WarningCode,
		resource.Error,
		audio,
		sampleRate,
		channels,
		sampleWidth,
		durationMS,
		sttLanguage,
		nonNilStrings(workerNotes),
		completedAt,
	)
	if err != nil {
		return mapPostgresWriteError("insert fragment attempt", err)
	}
	return nil
}

const bookSelect = `SELECT
	id, title, authors, format, chapters_count, fragments_count, status, created_at
FROM books`

const voiceSelect = `SELECT
	id, name, mode, format, content_type, size_bytes,
	has_reference_text, status, created_at
FROM voices`

const jobSelect = `SELECT
	id, book_id, voice_id, status, fragments_count, fragments_pending,
	fragments_ready, fragments_warnings, fragments_failed,
	omnivoice_num_steps, omnivoice_guidance_scale, omnivoice_speed,
	omnivoice_normalize_text, omnivoice_denoise, omnivoice_t_shift,
	omnivoice_layer_penalty_factor, omnivoice_position_temperature,
	omnivoice_class_temperature, omnivoice_preprocess_prompt,
	omnivoice_postprocess_output, omnivoice_audio_chunk_duration,
	omnivoice_audio_chunk_threshold, omnivoice_pad_duration,
	omnivoice_fade_duration, whisper_beam_size, whisper_patience,
	whisper_temperature, whisper_vad_filter, whisper_word_timestamps,
	automatic_warning_retries, created_at, updated_at
FROM jobs`

const fragmentSelect = `SELECT
	id, job_id, chapter_number, ordinal, text, stt_text, status,
	warning_code, error_message, attempt, updated_at
FROM job_fragments`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanBook(row rowScanner) (BookResource, error) {
	var resource BookResource
	err := row.Scan(
		&resource.ID,
		&resource.Title,
		&resource.Authors,
		&resource.Format,
		&resource.ChaptersCount,
		&resource.FragmentsCount,
		&resource.Status,
		&resource.CreatedAt,
	)
	resource.Authors = nonNilStrings(resource.Authors)
	return resource, err
}

func scanVoice(row rowScanner) (VoiceResource, error) {
	var resource VoiceResource
	err := row.Scan(
		&resource.ID,
		&resource.Name,
		&resource.Mode,
		&resource.Format,
		&resource.ContentType,
		&resource.SizeBytes,
		&resource.HasReferenceText,
		&resource.Status,
		&resource.CreatedAt,
	)
	return resource, err
}

func scanJob(row rowScanner) (JobResource, error) {
	var resource JobResource
	err := row.Scan(
		&resource.ID,
		&resource.BookID,
		&resource.VoiceID,
		&resource.Status,
		&resource.FragmentsCount,
		&resource.FragmentsPending,
		&resource.FragmentsReady,
		&resource.FragmentsWarnings,
		&resource.FragmentsFailed,
		&resource.GenerationSettings.OmniVoice.NumSteps,
		&resource.GenerationSettings.OmniVoice.GuidanceScale,
		&resource.GenerationSettings.OmniVoice.Speed,
		&resource.GenerationSettings.OmniVoice.NormalizeText,
		&resource.GenerationSettings.OmniVoice.Denoise,
		&resource.GenerationSettings.OmniVoice.TShift,
		&resource.GenerationSettings.OmniVoice.LayerPenaltyFactor,
		&resource.GenerationSettings.OmniVoice.PositionTemperature,
		&resource.GenerationSettings.OmniVoice.ClassTemperature,
		&resource.GenerationSettings.OmniVoice.PreprocessPrompt,
		&resource.GenerationSettings.OmniVoice.PostprocessOutput,
		&resource.GenerationSettings.OmniVoice.AudioChunkDuration,
		&resource.GenerationSettings.OmniVoice.AudioChunkThreshold,
		&resource.GenerationSettings.OmniVoice.PadDuration,
		&resource.GenerationSettings.OmniVoice.FadeDuration,
		&resource.GenerationSettings.Whisper.BeamSize,
		&resource.GenerationSettings.Whisper.Patience,
		&resource.GenerationSettings.Whisper.Temperature,
		&resource.GenerationSettings.Whisper.VADFilter,
		&resource.GenerationSettings.Whisper.WordTimestamps,
		&resource.GenerationSettings.AutomaticWarningRetries,
		&resource.CreatedAt,
		&resource.UpdatedAt,
	)
	return resource, err
}

func scanFragment(row rowScanner) (FragmentResource, error) {
	var resource FragmentResource
	err := row.Scan(
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
	)
	return resource, err
}

func nonNilStrings(values []string) []string {
	if len(values) == 0 {
		return make([]string, 0)
	}
	return slices.Clone(values)
}

func nonNilBytes(value []byte) []byte {
	if len(value) == 0 {
		return make([]byte, 0)
	}
	return slices.Clone(value)
}

func firstDuplicate(values []string) string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return value
		}
		seen[value] = struct{}{}
	}
	return ""
}

func rollback(tx pgx.Tx) {
	_ = tx.Rollback(context.Background())
}

func mapPostgresWriteError(action string, err error) error {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		return fmt.Errorf("%s: %w", action, err)
	}

	switch postgresError.Code {
	case "23505":
		return fmt.Errorf("%w: %s violates uniqueness", errConflict, action)
	case "23503":
		return fmt.Errorf("%w: %s references a missing resource", errNotFound, action)
	case "23502", "23514", "22001", "22P02":
		return fmt.Errorf("%w: %s contains invalid data", errInvalid, action)
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}

var _ repository = (*PostgresStore)(nil)
