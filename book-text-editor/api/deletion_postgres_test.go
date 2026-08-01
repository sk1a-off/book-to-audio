package api

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"book-text-editor/internal/book"
)

func TestPostgresStoreUserDeletionCascades(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := preparePostgresStore(t, ctx, databaseURL)
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)

	createdBook, err := store.createBook(ctx, "delete-book", book.Book{
		Title: "Удаляемая книга",
		Chapters: []book.Chapter{{
			Number:   1,
			Title:    "Глава",
			Segments: []string{"Фрагмент."},
		}},
	}, now)
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	createdVoice, err := store.createVoice(
		ctx,
		"delete-voice",
		"Удаляемый голос",
		"flac",
		"audio/flac",
		"Эталон.",
		[]byte("voice"),
		now,
	)
	if err != nil {
		t.Fatalf("create voice: %v", err)
	}
	_, _, err = store.createJob(
		ctx,
		"delete-job",
		createdBook.ID,
		createdVoice.ID,
		[]string{"delete-fragment"},
		[]string{"delete-revision"},
		defaultGenerationSettings(),
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	if deleted, err := store.deleteJobForUser(ctx, "delete-job"); deleted || !errors.Is(err, errConflict) {
		t.Fatalf("delete queued job = (%v, %v), want conflict", deleted, err)
	}
	if deleted, err := store.deleteVoice(ctx, createdVoice.ID); deleted || !errors.Is(err, errConflict) {
		t.Fatalf("delete referenced voice = (%v, %v), want conflict", deleted, err)
	}

	if _, err := store.pool.Exec(
		ctx,
		`UPDATE jobs
		 SET status = 'failed', fragments_pending = 0, fragments_failed = fragments_count
		 WHERE id = 'delete-job'`,
	); err != nil {
		t.Fatalf("mark job terminal: %v", err)
	}
	if _, err := store.pool.Exec(
		ctx,
		`INSERT INTO fragment_attempts (
			fragment_id, attempt, text, status, completed_at
		) VALUES ('delete-fragment', 1, 'Фрагмент.', 'failed', $1)`,
		now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("insert fragment attempt: %v", err)
	}

	if deleted, err := store.deleteJobForUser(ctx, "delete-job"); err != nil || !deleted {
		t.Fatalf("delete terminal job = (%v, %v), want true", deleted, err)
	}
	for table, query := range map[string]string{
		"jobs":              `SELECT count(*) FROM jobs WHERE id = 'delete-job'`,
		"job_fragments":     `SELECT count(*) FROM job_fragments WHERE job_id = 'delete-job'`,
		"fragment_attempts": `SELECT count(*) FROM fragment_attempts WHERE fragment_id = 'delete-fragment'`,
	} {
		var count int
		if err := store.pool.QueryRow(ctx, query).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows after job deletion = %d, want 0", table, count)
		}
	}
	if deleted, err := store.deleteVoice(ctx, createdVoice.ID); err != nil || !deleted {
		t.Fatalf("delete voice = (%v, %v), want true", deleted, err)
	}
}
