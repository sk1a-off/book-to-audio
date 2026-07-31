package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

type rewriteTaskRecord struct {
	Resource    RewriteTaskResource
	FragmentIDs []string
	Fragments   map[string]*RewriteTaskFragmentResource
}

func (s *memoryStore) createRewriteTask(
	_ context.Context,
	id, jobID string,
	requested []string,
	modelID, prompt string,
	temperature float64,
	topK int,
	topP, minP, repeatPenalty float64,
	maxTokens int,
	now time.Time,
) (RewriteTaskResource, rewriteTask, error) {
	if duplicate := firstDuplicate(requested); duplicate != "" {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: duplicate fragment id %q",
			errInvalid,
			duplicate,
		)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: job not found",
			errNotFound,
		)
	}
	if job.Resource.Status == JobStatusQueued ||
		job.Resource.Status == JobStatusRunning {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: generation job is active",
			errConflict,
		)
	}
	if id == "" || modelID == "" || prompt == "" ||
		math.IsNaN(temperature) || math.IsInf(temperature, 0) ||
		temperature < 0 || temperature > 1 ||
		topK < 0 || topK > 200 ||
		!validRewriteFloat(topP, 0, 1) ||
		!validRewriteFloat(minP, 0, 1) ||
		!validRewriteFloat(repeatPenalty, 0.5, 2) ||
		maxTokens < 16 || maxTokens > 2_048 {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: rewrite identity, model, prompt, and settings are invalid",
			errInvalid,
		)
	}
	if _, exists := s.rewriteTasks[id]; exists {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: duplicate rewrite id",
			errConflict,
		)
	}
	for _, candidate := range s.rewriteTasks {
		if candidate.Resource.JobID == jobID &&
			(candidate.Resource.Status == RewriteTaskStatusQueued ||
				candidate.Resource.Status == RewriteTaskStatusRunning) {
			return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
				"%w: job already has an active rewrite",
				errConflict,
			)
		}
	}

	selected := slices.Clone(requested)
	if len(selected) == 0 {
		for _, fragmentID := range job.FragmentIDs {
			if s.fragments[fragmentID].Resource.Status == FragmentStatusWarning {
				selected = append(selected, fragmentID)
			}
		}
	}
	if len(selected) == 0 {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: job has no warning fragments",
			errConflict,
		)
	}
	selectedSet := make(map[string]struct{}, len(selected))
	for _, fragmentID := range selected {
		fragment, exists := s.fragments[fragmentID]
		if !exists {
			return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
				"%w: fragment %q not found",
				errNotFound,
				fragmentID,
			)
		}
		if fragment.Resource.JobID != jobID {
			return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
				"%w: fragment %q does not belong to job",
				errInvalid,
				fragmentID,
			)
		}
		if fragment.Resource.Status != FragmentStatusWarning {
			return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
				"%w: fragment %q is not a warning",
				errInvalid,
				fragmentID,
			)
		}
		selectedSet[fragmentID] = struct{}{}
	}
	// Preserve book order even when the caller supplied IDs in another order.
	selected = selected[:0]
	for _, fragmentID := range job.FragmentIDs {
		if _, exists := selectedSet[fragmentID]; exists {
			selected = append(selected, fragmentID)
		}
	}

	resource := RewriteTaskResource{
		ID:               id,
		JobID:            jobID,
		ModelID:          modelID,
		Prompt:           prompt,
		Temperature:      temperature,
		TopK:             topK,
		TopP:             topP,
		MinP:             minP,
		RepeatPenalty:    repeatPenalty,
		MaxTokens:        maxTokens,
		Status:           RewriteTaskStatusQueued,
		FragmentsCount:   len(selected),
		FragmentsPending: len(selected),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	record := &rewriteTaskRecord{
		Resource:    resource,
		FragmentIDs: slices.Clone(selected),
		Fragments:   make(map[string]*RewriteTaskFragmentResource, len(selected)),
	}
	for index, fragmentID := range selected {
		record.Fragments[fragmentID] = &RewriteTaskFragmentResource{
			FragmentID: fragmentID,
			Ordinal:    index + 1,
			Status:     RewriteFragmentStatusQueued,
			UpdatedAt:  now,
		}
	}
	s.rewriteTasks[id] = record

	return resource, rewriteTask{
		ID:            id,
		JobID:         jobID,
		FragmentIDs:   slices.Clone(selected),
		ModelID:       modelID,
		Prompt:        prompt,
		Temperature:   temperature,
		TopK:          topK,
		TopP:          topP,
		MinP:          minP,
		RepeatPenalty: repeatPenalty,
		MaxTokens:     maxTokens,
	}, nil
}

func (s *memoryStore) deleteRewriteTask(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rewriteTasks, id)
	return nil
}

func (s *memoryStore) rewriteTask(
	_ context.Context,
	id string,
) (RewriteTaskResponse, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.rewriteTasks[id]
	if !ok {
		return RewriteTaskResponse{}, false, nil
	}
	return cloneMemoryRewriteTask(record), true, nil
}

func (s *memoryStore) startRewriteTask(
	_ context.Context,
	id string,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.rewriteTasks[id]
	if !ok {
		return fmt.Errorf("%w: rewrite task not found", errNotFound)
	}
	if record.Resource.Status != RewriteTaskStatusQueued {
		return fmt.Errorf("%w: rewrite task is not queued", errConflict)
	}
	record.Resource.Status = RewriteTaskStatusRunning
	record.Resource.UpdatedAt = now
	return nil
}

func (s *memoryStore) startRewriteFragment(
	_ context.Context,
	rewriteID, fragmentID string,
	now time.Time,
) (rewriteWorkItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.rewriteTasks[rewriteID]
	if !ok {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: rewrite task not found",
			errNotFound,
		)
	}
	if task.Resource.Status != RewriteTaskStatusRunning {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: rewrite task is not running",
			errConflict,
		)
	}
	child, ok := task.Fragments[fragmentID]
	if !ok {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: rewrite fragment not found",
			errNotFound,
		)
	}
	if child.Status != RewriteFragmentStatusQueued {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: rewrite fragment is not queued",
			errConflict,
		)
	}
	fragment, ok := s.fragments[fragmentID]
	if !ok {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	if fragment.Resource.JobID != task.Resource.JobID ||
		fragment.Resource.Status != FragmentStatusWarning {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: fragment is no longer a warning in this job",
			errConflict,
		)
	}
	child.Status = RewriteFragmentStatusRunning
	child.UpdatedAt = now
	task.Resource.UpdatedAt = now

	return rewriteWorkItem{
		RewriteID:     rewriteID,
		JobID:         task.Resource.JobID,
		Fragment:      fragment.Resource,
		ModelID:       task.Resource.ModelID,
		Prompt:        task.Resource.Prompt,
		Temperature:   task.Resource.Temperature,
		TopK:          task.Resource.TopK,
		TopP:          task.Resource.TopP,
		MinP:          task.Resource.MinP,
		RepeatPenalty: task.Resource.RepeatPenalty,
		MaxTokens:     task.Resource.MaxTokens,
	}, nil
}

func (s *memoryStore) completeRewriteFragment(
	_ context.Context,
	rewriteID, fragmentID, expectedText, revisionID string,
	result RewriterResult,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, child, fragment, err := s.lockedMemoryRewriteEntries(
		rewriteID,
		fragmentID,
	)
	if err != nil {
		return err
	}
	if task.Resource.Status != RewriteTaskStatusRunning ||
		child.Status != RewriteFragmentStatusRunning {
		return fmt.Errorf("%w: rewrite fragment is not running", errConflict)
	}
	if result.ModelID != task.Resource.ModelID ||
		result.ModelRevision == "" {
		return fmt.Errorf("%w: rewrite model identity changed", errInvalid)
	}
	if task.Resource.ModelRevision != "" &&
		task.Resource.ModelRevision != result.ModelRevision {
		return fmt.Errorf("%w: rewrite model revision changed", errConflict)
	}
	if fragment.Resource.Status != FragmentStatusWarning ||
		fragment.Resource.Text != expectedText {
		return fmt.Errorf(
			"%w: fragment changed while the model was running",
			errConflict,
		)
	}

	changed := result.RewrittenText != expectedText
	if changed {
		if revisionID == "" {
			return fmt.Errorf("%w: AI revision id is required", errInvalid)
		}
		if _, exists := s.textRevisionIDs[revisionID]; exists {
			return fmt.Errorf("%w: duplicate revision id", errConflict)
		}
		current, ok := currentTextRevision(fragment.TextRevisions)
		if !ok {
			return errors.New("fragment has no current text revision")
		}
		revision := newFragmentTextRevision(
			revisionID,
			fragmentID,
			current.RevisionNumber+1,
			FragmentTextRevisionSourceAI,
			result.RewrittenText,
			current.ID,
			result.Reason,
			now,
		)
		revision.ModelID = optionalStringPointer(task.Resource.ModelID)
		revision.ModelRevision = optionalStringPointer(result.ModelRevision)
		revision.Prompt = optionalStringPointer(task.Resource.Prompt)
		fragment.TextRevisions = append(fragment.TextRevisions, revision)
		s.textRevisionIDs[revisionID] = fragmentID

		fragment.Resource.Text = result.RewrittenText
		fragment.Resource.STTText = ""
		fragment.Resource.Status = FragmentStatusWarning
		fragment.Resource.WarningCode = "text_rewritten"
		fragment.Resource.Error = ""
		fragment.Resource.UpdatedAt = now
		clearMemoryFragmentAudio(fragment)
		recomputeJob(s.jobs[fragment.Resource.JobID], s.fragments, now)
	}

	task.Resource.ModelRevision = result.ModelRevision
	child.Status = RewriteFragmentStatusCompleted
	child.RevisionID = revisionID
	child.Changed = changed
	child.Reason = result.Reason
	child.Error = ""
	child.DurationMS = result.DurationMS
	child.ModelRevision = result.ModelRevision
	child.UpdatedAt = now
	recomputeMemoryRewriteTask(task, now)
	return nil
}

func (s *memoryStore) failRewriteFragment(
	_ context.Context,
	rewriteID, fragmentID, message string,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.rewriteTasks[rewriteID]
	if !ok {
		return fmt.Errorf("%w: rewrite task not found", errNotFound)
	}
	child, ok := task.Fragments[fragmentID]
	if !ok {
		return fmt.Errorf("%w: rewrite fragment not found", errNotFound)
	}
	if child.Status == RewriteFragmentStatusCompleted ||
		child.Status == RewriteFragmentStatusFailed {
		return nil
	}
	child.Status = RewriteFragmentStatusFailed
	child.Error = boundedMessage(message, maxRewriteFailureLength)
	child.UpdatedAt = now
	recomputeMemoryRewriteTask(task, now)
	return nil
}

func (s *memoryStore) finishRewriteTask(
	_ context.Context,
	rewriteID string,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.rewriteTasks[rewriteID]
	if !ok {
		return fmt.Errorf("%w: rewrite task not found", errNotFound)
	}
	recomputeMemoryRewriteTask(task, now)
	if task.Resource.FragmentsPending != 0 {
		return fmt.Errorf("%w: rewrite task still has pending fragments", errConflict)
	}
	return nil
}

func (s *memoryStore) lockedMemoryRewriteEntries(
	rewriteID, fragmentID string,
) (*rewriteTaskRecord, *RewriteTaskFragmentResource, *fragmentRecord, error) {
	task, ok := s.rewriteTasks[rewriteID]
	if !ok {
		return nil, nil, nil, fmt.Errorf(
			"%w: rewrite task not found",
			errNotFound,
		)
	}
	child, ok := task.Fragments[fragmentID]
	if !ok {
		return nil, nil, nil, fmt.Errorf(
			"%w: rewrite fragment not found",
			errNotFound,
		)
	}
	fragment, ok := s.fragments[fragmentID]
	if !ok {
		return nil, nil, nil, fmt.Errorf(
			"%w: fragment not found",
			errNotFound,
		)
	}
	return task, child, fragment, nil
}

func recomputeMemoryRewriteTask(task *rewriteTaskRecord, now time.Time) {
	task.Resource.FragmentsPending = 0
	task.Resource.FragmentsCompleted = 0
	task.Resource.FragmentsFailed = 0
	for _, fragmentID := range task.FragmentIDs {
		switch task.Fragments[fragmentID].Status {
		case RewriteFragmentStatusQueued, RewriteFragmentStatusRunning:
			task.Resource.FragmentsPending++
		case RewriteFragmentStatusCompleted:
			task.Resource.FragmentsCompleted++
		case RewriteFragmentStatusFailed:
			task.Resource.FragmentsFailed++
		}
	}
	switch {
	case task.Resource.FragmentsPending > 0:
		task.Resource.Status = RewriteTaskStatusRunning
	case task.Resource.FragmentsFailed == task.Resource.FragmentsCount:
		task.Resource.Status = RewriteTaskStatusFailed
	case task.Resource.FragmentsFailed > 0:
		task.Resource.Status = RewriteTaskStatusCompletedWithErrors
	default:
		task.Resource.Status = RewriteTaskStatusCompleted
	}
	task.Resource.UpdatedAt = now
}

func cloneMemoryRewriteTask(record *rewriteTaskRecord) RewriteTaskResponse {
	result := RewriteTaskResponse{
		RewriteTaskResource: record.Resource,
		Fragments: make(
			[]RewriteTaskFragmentResource,
			0,
			len(record.FragmentIDs),
		),
	}
	for _, fragmentID := range record.FragmentIDs {
		result.Fragments = append(result.Fragments, *record.Fragments[fragmentID])
	}
	return result
}

func (s *PostgresStore) createRewriteTask(
	ctx context.Context,
	id, jobID string,
	requested []string,
	modelID, prompt string,
	temperature float64,
	topK int,
	topP, minP, repeatPenalty float64,
	maxTokens int,
	now time.Time,
) (RewriteTaskResource, rewriteTask, error) {
	if id == "" || modelID == "" || prompt == "" ||
		math.IsNaN(temperature) || math.IsInf(temperature, 0) ||
		temperature < 0 || temperature > 1 ||
		topK < 0 || topK > 200 ||
		!validRewriteFloat(topP, 0, 1) ||
		!validRewriteFloat(minP, 0, 1) ||
		!validRewriteFloat(repeatPenalty, 0.5, 2) ||
		maxTokens < 16 || maxTokens > 2_048 {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: rewrite identity, model, prompt, and settings are invalid",
			errInvalid,
		)
	}
	if duplicate := firstDuplicate(requested); duplicate != "" {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: duplicate fragment id %q",
			errInvalid,
			duplicate,
		)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"begin create rewrite transaction: %w",
			err,
		)
	}
	defer rollback(tx)

	job, err := scanJob(
		tx.QueryRow(ctx, jobSelect+` WHERE id = $1 FOR UPDATE`, jobID),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: job not found",
			errNotFound,
		)
	}
	if err != nil {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"lock rewrite job: %w",
			err,
		)
	}
	if job.Status == JobStatusQueued || job.Status == JobStatusRunning {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: generation job is active",
			errConflict,
		)
	}
	var active bool
	if err := tx.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM rewrite_tasks
			WHERE job_id = $1 AND status IN ('queued', 'running')
		)`,
		jobID,
	).Scan(&active); err != nil {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"check active rewrite: %w",
			err,
		)
	}
	if active {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: job already has an active rewrite",
			errConflict,
		)
	}

	selected, err := selectPostgresRewriteFragments(
		ctx,
		tx,
		jobID,
		requested,
	)
	if err != nil {
		return RewriteTaskResource{}, rewriteTask{}, err
	}
	if len(selected) == 0 {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"%w: job has no warning fragments",
			errConflict,
		)
	}

	resource := RewriteTaskResource{
		ID:               id,
		JobID:            jobID,
		ModelID:          modelID,
		Prompt:           prompt,
		Temperature:      temperature,
		TopK:             topK,
		TopP:             topP,
		MinP:             minP,
		RepeatPenalty:    repeatPenalty,
		MaxTokens:        maxTokens,
		Status:           RewriteTaskStatusQueued,
		FragmentsCount:   len(selected),
		FragmentsPending: len(selected),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	_, err = tx.Exec(
		ctx,
		`INSERT INTO rewrite_tasks (
			id, job_id, model_id, prompt, temperature, top_k, top_p,
			min_p, repeat_penalty, max_tokens,
			status, fragments_count,
			fragments_pending, fragments_completed, fragments_failed,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, 0, 0, $14, $14
		)`,
		resource.ID,
		resource.JobID,
		resource.ModelID,
		resource.Prompt,
		resource.Temperature,
		resource.TopK,
		resource.TopP,
		resource.MinP,
		resource.RepeatPenalty,
		resource.MaxTokens,
		resource.Status,
		resource.FragmentsCount,
		resource.FragmentsPending,
		now,
	)
	if err != nil {
		return RewriteTaskResource{}, rewriteTask{}, mapPostgresWriteError(
			"insert rewrite task",
			err,
		)
	}
	for index, fragment := range selected {
		_, err = tx.Exec(
			ctx,
			`INSERT INTO rewrite_task_fragments (
				rewrite_id, job_id, fragment_id, ordinal, status, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6)`,
			resource.ID,
			resource.JobID,
			fragment.ID,
			index+1,
			RewriteFragmentStatusQueued,
			now,
		)
		if err != nil {
			return RewriteTaskResource{}, rewriteTask{}, mapPostgresWriteError(
				"insert rewrite fragment",
				err,
			)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RewriteTaskResource{}, rewriteTask{}, fmt.Errorf(
			"commit create rewrite transaction: %w",
			err,
		)
	}

	fragmentIDs := make([]string, 0, len(selected))
	for _, fragment := range selected {
		fragmentIDs = append(fragmentIDs, fragment.ID)
	}
	return resource, rewriteTask{
		ID:            id,
		JobID:         jobID,
		FragmentIDs:   fragmentIDs,
		ModelID:       modelID,
		Prompt:        prompt,
		Temperature:   temperature,
		TopK:          topK,
		TopP:          topP,
		MinP:          minP,
		RepeatPenalty: repeatPenalty,
		MaxTokens:     maxTokens,
	}, nil
}

func selectPostgresRewriteFragments(
	ctx context.Context,
	tx pgx.Tx,
	jobID string,
	requested []string,
) ([]FragmentResource, error) {
	query := fragmentSelect + `
		WHERE job_id = $1 AND status = 'warning'
		ORDER BY ordinal ASC, id ASC
		FOR UPDATE`
	arguments := []any{jobID}
	if len(requested) > 0 {
		query = fragmentSelect + `
			WHERE job_id = $1 AND id = ANY($2::TEXT[])
			ORDER BY ordinal ASC, id ASC
			FOR UPDATE`
		arguments = append(arguments, requested)
	}
	rows, err := tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query rewrite fragments: %w", err)
	}
	defer rows.Close()

	result := make([]FragmentResource, 0, len(requested))
	for rows.Next() {
		fragment, scanErr := scanFragment(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan rewrite fragment: %w", scanErr)
		}
		if fragment.Status != FragmentStatusWarning {
			return nil, fmt.Errorf(
				"%w: fragment %q is not a warning",
				errInvalid,
				fragment.ID,
			)
		}
		result = append(result, fragment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rewrite fragments: %w", err)
	}
	if len(requested) > 0 && len(result) != len(requested) {
		return nil, fmt.Errorf(
			"%w: selected fragment is missing, belongs to another job, or is not a warning",
			errInvalid,
		)
	}
	return result, nil
}

func (s *PostgresStore) deleteRewriteTask(
	ctx context.Context,
	id string,
) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM rewrite_tasks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete rewrite task: %w", err)
	}
	return nil
}

func (s *PostgresStore) rewriteTask(
	ctx context.Context,
	id string,
) (RewriteTaskResponse, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return RewriteTaskResponse{}, false, fmt.Errorf(
			"begin rewrite snapshot: %w",
			err,
		)
	}
	defer rollback(tx)

	resource, err := scanRewriteTask(
		tx.QueryRow(ctx, rewriteTaskSelect+` WHERE id = $1`, id),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RewriteTaskResponse{}, false, nil
	}
	if err != nil {
		return RewriteTaskResponse{}, false, fmt.Errorf(
			"query rewrite task: %w",
			err,
		)
	}
	rows, err := tx.Query(
		ctx,
		rewriteFragmentSelect+`
		 WHERE rewrite_id = $1
		 ORDER BY ordinal ASC, fragment_id ASC`,
		id,
	)
	if err != nil {
		return RewriteTaskResponse{}, false, fmt.Errorf(
			"query rewrite task fragments: %w",
			err,
		)
	}
	fragments := make([]RewriteTaskFragmentResource, 0, resource.FragmentsCount)
	for rows.Next() {
		fragment, scanErr := scanRewriteFragment(rows)
		if scanErr != nil {
			rows.Close()
			return RewriteTaskResponse{}, false, fmt.Errorf(
				"scan rewrite task fragment: %w",
				scanErr,
			)
		}
		fragments = append(fragments, fragment)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RewriteTaskResponse{}, false, fmt.Errorf(
			"iterate rewrite task fragments: %w",
			err,
		)
	}
	if len(fragments) != resource.FragmentsCount {
		return RewriteTaskResponse{}, false, errors.New(
			"rewrite task fragment count does not match task",
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return RewriteTaskResponse{}, false, fmt.Errorf(
			"commit rewrite snapshot: %w",
			err,
		)
	}
	return RewriteTaskResponse{
		RewriteTaskResource: resource,
		Fragments:           fragments,
	}, true, nil
}

func (s *PostgresStore) startRewriteTask(
	ctx context.Context,
	id string,
	now time.Time,
) error {
	command, err := s.pool.Exec(
		ctx,
		`UPDATE rewrite_tasks
		 SET status = $2, updated_at = $3
		 WHERE id = $1 AND status = $4`,
		id,
		RewriteTaskStatusRunning,
		now,
		RewriteTaskStatusQueued,
	)
	if err != nil {
		return mapPostgresWriteError("start rewrite task", err)
	}
	if command.RowsAffected() != 1 {
		var exists bool
		if err := s.pool.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM rewrite_tasks WHERE id = $1)`,
			id,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check rewrite task: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: rewrite task not found", errNotFound)
		}
		return fmt.Errorf("%w: rewrite task is not queued", errConflict)
	}
	return nil
}

func (s *PostgresStore) startRewriteFragment(
	ctx context.Context,
	rewriteID, fragmentID string,
	now time.Time,
) (rewriteWorkItem, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return rewriteWorkItem{}, fmt.Errorf(
			"begin start rewrite fragment: %w",
			err,
		)
	}
	defer rollback(tx)

	task, err := scanRewriteTask(
		tx.QueryRow(
			ctx,
			rewriteTaskSelect+` WHERE id = $1 FOR UPDATE`,
			rewriteID,
		),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: rewrite task not found",
			errNotFound,
		)
	}
	if err != nil {
		return rewriteWorkItem{}, fmt.Errorf("lock rewrite task: %w", err)
	}
	if task.Status != RewriteTaskStatusRunning {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: rewrite task is not running",
			errConflict,
		)
	}
	child, err := scanRewriteFragment(
		tx.QueryRow(
			ctx,
			rewriteFragmentSelect+`
			 WHERE rewrite_id = $1 AND fragment_id = $2
			 FOR UPDATE`,
			rewriteID,
			fragmentID,
		),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: rewrite fragment not found",
			errNotFound,
		)
	}
	if err != nil {
		return rewriteWorkItem{}, fmt.Errorf("lock rewrite fragment: %w", err)
	}
	if child.Status != RewriteFragmentStatusQueued {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: rewrite fragment is not queued",
			errConflict,
		)
	}
	fragment, err := scanFragment(
		tx.QueryRow(
			ctx,
			fragmentSelect+` WHERE id = $1 FOR UPDATE`,
			fragmentID,
		),
	)
	if err != nil {
		return rewriteWorkItem{}, fmt.Errorf("lock fragment for rewrite: %w", err)
	}
	if fragment.JobID != task.JobID ||
		fragment.Status != FragmentStatusWarning {
		return rewriteWorkItem{}, fmt.Errorf(
			"%w: fragment is no longer a warning in this job",
			errConflict,
		)
	}
	_, err = tx.Exec(
		ctx,
		`UPDATE rewrite_task_fragments
		 SET status = $3, updated_at = $4
		 WHERE rewrite_id = $1 AND fragment_id = $2`,
		rewriteID,
		fragmentID,
		RewriteFragmentStatusRunning,
		now,
	)
	if err != nil {
		return rewriteWorkItem{}, mapPostgresWriteError(
			"start rewrite fragment",
			err,
		)
	}
	if _, err := tx.Exec(
		ctx,
		`UPDATE rewrite_tasks SET updated_at = $2 WHERE id = $1`,
		rewriteID,
		now,
	); err != nil {
		return rewriteWorkItem{}, mapPostgresWriteError(
			"touch rewrite task",
			err,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return rewriteWorkItem{}, fmt.Errorf(
			"commit start rewrite fragment: %w",
			err,
		)
	}
	return rewriteWorkItem{
		RewriteID:     rewriteID,
		JobID:         task.JobID,
		Fragment:      fragment,
		ModelID:       task.ModelID,
		Prompt:        task.Prompt,
		Temperature:   task.Temperature,
		TopK:          task.TopK,
		TopP:          task.TopP,
		MinP:          task.MinP,
		RepeatPenalty: task.RepeatPenalty,
		MaxTokens:     task.MaxTokens,
	}, nil
}

func (s *PostgresStore) completeRewriteFragment(
	ctx context.Context,
	rewriteID, fragmentID, expectedText, revisionID string,
	result RewriterResult,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin complete rewrite fragment: %w", err)
	}
	defer rollback(tx)

	task, err := scanRewriteTask(
		tx.QueryRow(
			ctx,
			rewriteTaskSelect+` WHERE id = $1 FOR UPDATE`,
			rewriteID,
		),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: rewrite task not found", errNotFound)
	}
	if err != nil {
		return fmt.Errorf("lock rewrite task: %w", err)
	}
	if task.Status != RewriteTaskStatusRunning {
		return fmt.Errorf("%w: rewrite task is not running", errConflict)
	}
	if result.ModelID != task.ModelID || result.ModelRevision == "" {
		return fmt.Errorf("%w: rewrite model identity changed", errInvalid)
	}
	if task.ModelRevision != "" &&
		task.ModelRevision != result.ModelRevision {
		return fmt.Errorf("%w: rewrite model revision changed", errConflict)
	}
	if err := lockJob(ctx, tx, task.JobID); err != nil {
		return err
	}
	child, err := scanRewriteFragment(
		tx.QueryRow(
			ctx,
			rewriteFragmentSelect+`
			 WHERE rewrite_id = $1 AND fragment_id = $2
			 FOR UPDATE`,
			rewriteID,
			fragmentID,
		),
	)
	if err != nil {
		return fmt.Errorf("lock rewrite fragment: %w", err)
	}
	if child.Status != RewriteFragmentStatusRunning {
		return fmt.Errorf("%w: rewrite fragment is not running", errConflict)
	}
	fragment, err := scanFragment(
		tx.QueryRow(
			ctx,
			fragmentSelect+` WHERE id = $1 FOR UPDATE`,
			fragmentID,
		),
	)
	if err != nil {
		return fmt.Errorf("lock rewritten fragment: %w", err)
	}
	if fragment.JobID != task.JobID ||
		fragment.Status != FragmentStatusWarning ||
		fragment.Text != expectedText {
		return fmt.Errorf(
			"%w: fragment changed while the model was running",
			errConflict,
		)
	}

	changed := result.RewrittenText != expectedText
	if changed {
		if revisionID == "" {
			return fmt.Errorf("%w: AI revision id is required", errInvalid)
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
			return fmt.Errorf("query current text revision: %w", err)
		}
		revision := newFragmentTextRevision(
			revisionID,
			fragmentID,
			current.RevisionNumber+1,
			FragmentTextRevisionSourceAI,
			result.RewrittenText,
			current.ID,
			result.Reason,
			now,
		)
		revision.ModelID = optionalStringPointer(task.ModelID)
		revision.ModelRevision = optionalStringPointer(result.ModelRevision)
		revision.Prompt = optionalStringPointer(task.Prompt)
		if err := insertTextRevision(ctx, tx, revision); err != nil {
			return err
		}
		_, err = tx.Exec(
			ctx,
			`UPDATE job_fragments
			 SET text = $2,
			     stt_text = '',
			     status = $3,
			     warning_code = 'text_rewritten',
			     error_message = '',
			     audio_pcm = ''::BYTEA,
			     sample_rate = 0,
			     channels = 0,
			     sample_width = 0,
			     duration_ms = 0,
			     stt_language = '',
			     worker_notes = ARRAY[]::TEXT[],
			     updated_at = $4
			 WHERE id = $1`,
			fragmentID,
			result.RewrittenText,
			FragmentStatusWarning,
			now,
		)
		if err != nil {
			return mapPostgresWriteError("update AI rewritten fragment", err)
		}
		if err := recomputePostgresJob(ctx, tx, task.JobID, now); err != nil {
			return err
		}
	}

	_, err = tx.Exec(
		ctx,
		`UPDATE rewrite_task_fragments
		 SET status = $3,
		     revision_id = $4,
		     changed = $5,
		     reason = $6,
		     error_message = '',
		     duration_ms = $7,
		     model_revision = $8,
		     updated_at = $9
		 WHERE rewrite_id = $1 AND fragment_id = $2`,
		rewriteID,
		fragmentID,
		RewriteFragmentStatusCompleted,
		nilIfEmpty(revisionID),
		changed,
		result.Reason,
		result.DurationMS,
		result.ModelRevision,
		now,
	)
	if err != nil {
		return mapPostgresWriteError("complete rewrite fragment", err)
	}
	if _, err := tx.Exec(
		ctx,
		`UPDATE rewrite_tasks
		 SET model_revision = CASE
		         WHEN model_revision = '' THEN $2
		         ELSE model_revision
		     END,
		     updated_at = $3
		 WHERE id = $1`,
		rewriteID,
		result.ModelRevision,
		now,
	); err != nil {
		return mapPostgresWriteError("record rewrite model revision", err)
	}
	if err := recomputePostgresRewriteTask(ctx, tx, rewriteID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit complete rewrite fragment: %w", err)
	}
	return nil
}

func (s *PostgresStore) failRewriteFragment(
	ctx context.Context,
	rewriteID, fragmentID, message string,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin fail rewrite fragment: %w", err)
	}
	defer rollback(tx)

	command, err := tx.Exec(
		ctx,
		`UPDATE rewrite_task_fragments
		 SET status = $3, error_message = $4, updated_at = $5
		 WHERE rewrite_id = $1
		   AND fragment_id = $2
		   AND status IN ('queued', 'running')`,
		rewriteID,
		fragmentID,
		RewriteFragmentStatusFailed,
		boundedMessage(message, maxRewriteFailureLength),
		now,
	)
	if err != nil {
		return mapPostgresWriteError("fail rewrite fragment", err)
	}
	if command.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(
			ctx,
			`SELECT EXISTS (
				SELECT 1 FROM rewrite_task_fragments
				WHERE rewrite_id = $1 AND fragment_id = $2
			)`,
			rewriteID,
			fragmentID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check rewrite fragment: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: rewrite fragment not found", errNotFound)
		}
		return nil
	}
	if err := recomputePostgresRewriteTask(ctx, tx, rewriteID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fail rewrite fragment: %w", err)
	}
	return nil
}

func (s *PostgresStore) finishRewriteTask(
	ctx context.Context,
	rewriteID string,
	now time.Time,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin finish rewrite task: %w", err)
	}
	defer rollback(tx)
	if err := recomputePostgresRewriteTask(ctx, tx, rewriteID, now); err != nil {
		return err
	}
	var pending int
	if err := tx.QueryRow(
		ctx,
		`SELECT fragments_pending FROM rewrite_tasks WHERE id = $1`,
		rewriteID,
	).Scan(&pending); errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: rewrite task not found", errNotFound)
	} else if err != nil {
		return fmt.Errorf("query finished rewrite task: %w", err)
	}
	if pending != 0 {
		return fmt.Errorf("%w: rewrite task still has pending fragments", errConflict)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit finish rewrite task: %w", err)
	}
	return nil
}

func recomputePostgresRewriteTask(
	ctx context.Context,
	tx pgx.Tx,
	rewriteID string,
	now time.Time,
) error {
	command, err := tx.Exec(
		ctx,
		`WITH counters AS (
			SELECT
				count(*) FILTER (
					WHERE status IN ('queued', 'running')
				)::INTEGER AS pending,
				count(*) FILTER (WHERE status = 'completed')::INTEGER
					AS completed,
				count(*) FILTER (WHERE status = 'failed')::INTEGER AS failed
			FROM rewrite_task_fragments
			WHERE rewrite_id = $1
		)
		UPDATE rewrite_tasks AS task
		SET fragments_pending = counters.pending,
		    fragments_completed = counters.completed,
		    fragments_failed = counters.failed,
		    status = CASE
		        WHEN counters.pending > 0 THEN 'running'
		        WHEN counters.failed = task.fragments_count THEN 'failed'
		        WHEN counters.failed > 0 THEN 'completed_with_errors'
		        ELSE 'completed'
		    END,
		    updated_at = $2
		FROM counters
		WHERE task.id = $1`,
		rewriteID,
		now,
	)
	if err != nil {
		return mapPostgresWriteError("recompute rewrite task", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: rewrite task not found", errNotFound)
	}
	return nil
}

func scanRewriteTask(row rowScanner) (RewriteTaskResource, error) {
	var resource RewriteTaskResource
	err := row.Scan(
		&resource.ID,
		&resource.JobID,
		&resource.ModelID,
		&resource.ModelRevision,
		&resource.Prompt,
		&resource.Temperature,
		&resource.TopK,
		&resource.TopP,
		&resource.MinP,
		&resource.RepeatPenalty,
		&resource.MaxTokens,
		&resource.Status,
		&resource.FragmentsCount,
		&resource.FragmentsPending,
		&resource.FragmentsCompleted,
		&resource.FragmentsFailed,
		&resource.Error,
		&resource.CreatedAt,
		&resource.UpdatedAt,
	)
	return resource, err
}

func scanRewriteFragment(
	row rowScanner,
) (RewriteTaskFragmentResource, error) {
	var resource RewriteTaskFragmentResource
	var revisionID *string
	err := row.Scan(
		&resource.FragmentID,
		&resource.Ordinal,
		&resource.Status,
		&revisionID,
		&resource.Changed,
		&resource.Reason,
		&resource.Error,
		&resource.DurationMS,
		&resource.ModelRevision,
		&resource.UpdatedAt,
	)
	if revisionID != nil {
		resource.RevisionID = *revisionID
	}
	return resource, err
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const rewriteTaskSelect = `SELECT
	id, job_id, model_id, model_revision, prompt, temperature,
	top_k, top_p, min_p, repeat_penalty, max_tokens, status,
	fragments_count, fragments_pending, fragments_completed,
	fragments_failed, error_message, created_at, updated_at
FROM rewrite_tasks`

const rewriteFragmentSelect = `SELECT
	fragment_id, ordinal, status, revision_id, changed, reason,
	error_message, duration_ms, model_revision, updated_at
FROM rewrite_task_fragments`

func validRewriteFloat(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) &&
		!math.IsInf(value, 0) &&
		value >= minimum &&
		value <= maximum
}
