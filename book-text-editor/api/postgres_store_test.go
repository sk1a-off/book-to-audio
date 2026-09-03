package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"book-text-editor/internal/book"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOpenPostgresStoreRejectsEmptyURL(t *testing.T) {
	t.Parallel()

	if _, err := OpenPostgresStore(context.Background(), " "); err == nil {
		t.Fatal("OpenPostgresStore() error = nil, want invalid URL error")
	}
}

func TestPostgresStoreIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := preparePostgresStore(t, ctx, databaseURL)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

	var chapterIndexDefinition string
	if err := store.pool.QueryRow(
		ctx,
		`SELECT indexdef
		 FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND indexname = 'job_fragments_chapter_archive_idx'`,
	).Scan(&chapterIndexDefinition); err != nil {
		t.Fatalf("query chapter archive index: %v", err)
	}
	if !strings.Contains(
		chapterIndexDefinition,
		"(job_id, chapter_number, ordinal)",
	) {
		t.Fatalf(
			"chapter archive index = %q, want job/chapter/ordinal",
			chapterIndexDefinition,
		)
	}

	parsedBook := book.Book{
		Title:   "Книга",
		Authors: []string{"Первый Автор", "Второй Автор"},
		Chapters: []book.Chapter{
			{
				Number:   1,
				Title:    "Первая глава",
				Segments: []string{"Первый фрагмент.", "Второй фрагмент."},
			},
			{
				Number:   2,
				Title:    "Вторая глава",
				Segments: []string{"Третий фрагмент."},
			},
		},
	}
	createdBook, err := store.createBook(ctx, "book-1", parsedBook, now)
	if err != nil {
		t.Fatalf("createBook() error = %v", err)
	}
	if createdBook.FragmentsCount != 3 || createdBook.ChaptersCount != 2 {
		t.Fatalf("createBook() = %+v, want 2 chapters and 3 fragments", createdBook)
	}
	gotBook, ok, err := store.book(ctx, "book-1")
	if err != nil || !ok {
		t.Fatalf("book() = (%+v, %v, %v), want stored book", gotBook, ok, err)
	}
	if !reflect.DeepEqual(gotBook.Authors, parsedBook.Authors) {
		t.Fatalf("book authors = %#v, want %#v", gotBook.Authors, parsedBook.Authors)
	}
	if _, err := store.createBook(ctx, "book-1", parsedBook, now); !errors.Is(err, errConflict) {
		t.Fatalf("duplicate createBook() error = %v, want errConflict", err)
	}

	_, err = store.createVoice(
		ctx,
		"voice-b",
		"Второй голос",
		"flac",
		"audio/flac",
		"Эталон.",
		[]byte("voice-b"),
		now,
	)
	if err != nil {
		t.Fatalf("createVoice(voice-b) error = %v", err)
	}
	createdVoice, err := store.createVoice(
		ctx,
		"voice-a",
		"Основной голос",
		"flac",
		"audio/flac",
		"Эталонный текст.",
		[]byte("voice-a"),
		now,
	)
	if err != nil {
		t.Fatalf("createVoice(voice-a) error = %v", err)
	}
	voices, err := store.voices(ctx)
	if err != nil {
		t.Fatalf("voices() error = %v", err)
	}
	if len(voices) != 2 || voices[0].ID != "voice-a" || voices[1].ID != "voice-b" {
		t.Fatalf("voices() order = %#v, want voice-a then voice-b", voices)
	}
	gotVoice, ok, err := store.voice(ctx, createdVoice.ID)
	if err != nil || !ok ||
		gotVoice.ID != createdVoice.ID ||
		gotVoice.Name != createdVoice.Name ||
		gotVoice.Mode != createdVoice.Mode ||
		gotVoice.Format != createdVoice.Format ||
		gotVoice.ContentType != createdVoice.ContentType ||
		gotVoice.SizeBytes != createdVoice.SizeBytes ||
		gotVoice.HasReferenceText != createdVoice.HasReferenceText ||
		gotVoice.Status != createdVoice.Status ||
		!gotVoice.CreatedAt.Equal(createdVoice.CreatedAt) {
		t.Fatalf("voice() = (%+v, %v, %v), want %+v", gotVoice, ok, err, createdVoice)
	}
	gotPayload, ok, err := store.voicePayload(ctx, createdVoice.ID)
	if err != nil || !ok ||
		gotPayload.Resource.ID != createdVoice.ID ||
		gotPayload.Resource.ContentType != createdVoice.ContentType ||
		!gotPayload.Resource.CreatedAt.Equal(createdVoice.CreatedAt) ||
		gotPayload.ReferenceText != "Эталонный текст." ||
		string(gotPayload.Audio) != "voice-a" {
		t.Fatalf("voicePayload() = (%+v, %v, %v)", gotPayload, ok, err)
	}

	fragmentIDs := []string{"fragment-1", "fragment-2", "fragment-3"}
	generationSettings := defaultGenerationSettings()
	generationSettings.OmniVoice.NumSteps = 48
	generationSettings.OmniVoice.GuidanceScale = 3.25
	generationSettings.OmniVoice.Speed = 1.2
	generationSettings.OmniVoice.NormalizeText = false
	generationSettings.OmniVoice.Denoise = false
	generationSettings.OmniVoice.TShift = 0.25
	generationSettings.OmniVoice.LayerPenaltyFactor = 4.25
	generationSettings.OmniVoice.PositionTemperature = 3.75
	generationSettings.OmniVoice.ClassTemperature = 0.5
	generationSettings.OmniVoice.PreprocessPrompt = false
	generationSettings.OmniVoice.PostprocessOutput = false
	generationSettings.OmniVoice.AudioChunkDuration = 12.5
	generationSettings.OmniVoice.AudioChunkThreshold = 25.5
	generationSettings.OmniVoice.PadDuration = 0
	generationSettings.OmniVoice.FadeDuration = 0.05
	generationSettings.Whisper.BeamSize = 7
	generationSettings.Whisper.Patience = 1.4
	generationSettings.Whisper.Temperature = 0.25
	generationSettings.Whisper.VADFilter = true
	generationSettings.Whisper.WordTimestamps = false
	generationSettings.AutomaticWarningRetries = 4
	generationSettings.Pronunciation = PronunciationGenerationSettings{
		Enabled: true,
		Rules:   "старинный замок => старинный за́мок",
	}
	createdJob, task, err := store.createJob(
		ctx,
		"job-1",
		createdBook.ID,
		createdVoice.ID,
		fragmentIDs,
		[]string{"revision-1", "revision-2", "revision-3"},
		generationSettings,
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("createJob() error = %v", err)
	}
	if createdJob.Status != JobStatusQueued ||
		createdJob.FragmentsPending != len(fragmentIDs) ||
		createdJob.GenerationSettings != generationSettings {
		t.Fatalf("createJob() = %+v, want queued job", createdJob)
	}
	if !reflect.DeepEqual(task.FragmentIDs, fragmentIDs) ||
		task.Settings != generationSettings {
		t.Fatalf("task fragment IDs = %#v, want %#v", task.FragmentIDs, fragmentIDs)
	}
	if _, err := store.jobChapterCatalog(
		ctx,
		"missing-job",
	); !errors.Is(err, errNotFound) {
		t.Fatalf(
			"jobChapterCatalog(missing) error = %v, want errNotFound",
			err,
		)
	}
	if _, err := store.chapterArchiveSnapshot(
		ctx,
		"missing-job",
		1,
	); !errors.Is(err, errNotFound) {
		t.Fatalf(
			"chapterArchiveSnapshot(missing job) error = %v, want errNotFound",
			err,
		)
	}
	if _, err := store.jobChapterCatalog(
		ctx,
		createdJob.ID,
	); !errors.Is(err, errConflict) {
		t.Fatalf(
			"jobChapterCatalog(queued) error = %v, want errConflict",
			err,
		)
	}

	item, err := store.workItem(ctx, "fragment-1")
	if err != nil {
		t.Fatalf("workItem() error = %v", err)
	}
	if item.Resource.Text != "Первый фрагмент." ||
		item.Voice.ReferenceText != "Эталонный текст." ||
		string(item.Voice.Audio) != "voice-a" {
		t.Fatalf("workItem() = %+v, want source text and voice payload", item)
	}

	if err := store.startTask(ctx, task, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("startTask() error = %v", err)
	}
	for index, fragmentID := range fragmentIDs {
		started, startErr := store.startFragment(
			ctx,
			fragmentID,
			now.Add(time.Duration(3+index)*time.Minute),
		)
		if startErr != nil {
			t.Fatalf("startFragment(%q) error = %v", fragmentID, startErr)
		}
		if started.Attempt != 1 || started.Status != FragmentStatusGenerating {
			t.Fatalf("startFragment(%q) = %+v, want first attempt", fragmentID, started)
		}
	}

	if err := store.completeFragment(
		ctx,
		"fragment-1",
		testFragmentResult([]byte{1, 0}, "", "Первый фрагмент."),
		now.Add(6*time.Minute),
	); err != nil {
		t.Fatalf("completeFragment(fragment-1) error = %v", err)
	}
	assertPostgresJobState(
		t,
		ctx,
		store,
		createdJob.ID,
		JobStatusRunning,
		2,
		1,
		0,
		0,
	)
	if err := store.failFragment(
		ctx,
		"fragment-1",
		"late failure",
		now.Add(6*time.Minute+time.Second),
	); !errors.Is(err, errConflict) {
		t.Fatalf("late failFragment() error = %v, want errConflict", err)
	}
	if err := store.completeFragment(
		ctx,
		"fragment-2",
		testFragmentResult(
			[]byte{2, 0},
			"transcript_mismatch",
			"Другой фрагмент.",
		),
		now.Add(7*time.Minute),
	); err != nil {
		t.Fatalf("completeFragment(fragment-2) error = %v", err)
	}
	assertPostgresJobState(
		t,
		ctx,
		store,
		createdJob.ID,
		JobStatusRunning,
		1,
		1,
		1,
		0,
	)
	if err := store.failFragment(
		ctx,
		"fragment-3",
		"worker unavailable",
		now.Add(8*time.Minute),
	); err != nil {
		t.Fatalf("failFragment(fragment-3) error = %v", err)
	}
	assertPostgresJobState(
		t,
		ctx,
		store,
		createdJob.ID,
		JobStatusFailed,
		0,
		1,
		1,
		1,
	)
	if err := store.finishTask(ctx, createdJob.ID, now.Add(9*time.Minute)); err != nil {
		t.Fatalf("finishTask() error = %v", err)
	}

	failedJob, ok, err := store.job(ctx, createdJob.ID)
	if err != nil || !ok {
		t.Fatalf("job() = (%+v, %v, %v), want job", failedJob, ok, err)
	}
	if failedJob.Status != JobStatusFailed ||
		failedJob.FragmentsReady != 1 ||
		failedJob.FragmentsWarnings != 1 ||
		failedJob.FragmentsFailed != 1 {
		t.Fatalf("failed job = %+v, want one ready/warning/failed", failedJob)
	}

	issues, ok, err := store.jobIssues(ctx, createdJob.ID)
	if err != nil || !ok {
		t.Fatalf("jobIssues() = (%#v, %v, %v), want issues", issues, ok, err)
	}
	if len(issues) != 2 ||
		issues[0].ID != "fragment-2" ||
		issues[1].ID != "fragment-3" {
		t.Fatalf("jobIssues() = %#v, want deterministic fragment order", issues)
	}

	createdRewrite, rewriteTask, err := store.createRewriteTask(
		ctx,
		"rewrite-1",
		createdJob.ID,
		[]string{"fragment-2"},
		"qwen-test",
		"Сохрани всю фразу.",
		0.25,
		12,
		0.7,
		0.05,
		1.1,
		320,
		now.Add(9*time.Minute+time.Second),
	)
	if err != nil {
		t.Fatalf("createRewriteTask() error = %v", err)
	}
	if createdRewrite.Temperature != 0.25 ||
		createdRewrite.TopK != 12 ||
		createdRewrite.TopP != 0.7 ||
		createdRewrite.MinP != 0.05 ||
		createdRewrite.RepeatPenalty != 1.1 ||
		createdRewrite.MaxTokens != 320 ||
		rewriteTask.Temperature != createdRewrite.Temperature ||
		rewriteTask.TopK != createdRewrite.TopK ||
		rewriteTask.TopP != createdRewrite.TopP ||
		rewriteTask.MinP != createdRewrite.MinP ||
		rewriteTask.RepeatPenalty != createdRewrite.RepeatPenalty ||
		rewriteTask.MaxTokens != createdRewrite.MaxTokens {
		t.Fatalf(
			"rewrite settings were not snapshotted: resource=%+v task=%+v",
			createdRewrite,
			rewriteTask,
		)
	}
	if err := store.startRewriteTask(
		ctx,
		createdRewrite.ID,
		now.Add(9*time.Minute+2*time.Second),
	); err != nil {
		t.Fatalf("startRewriteTask() error = %v", err)
	}
	rewriteItem, err := store.startRewriteFragment(
		ctx,
		createdRewrite.ID,
		"fragment-2",
		now.Add(9*time.Minute+3*time.Second),
	)
	if err != nil {
		t.Fatalf("startRewriteFragment() error = %v", err)
	}
	if rewriteItem.Temperature != createdRewrite.Temperature ||
		rewriteItem.TopK != createdRewrite.TopK ||
		rewriteItem.TopP != createdRewrite.TopP ||
		rewriteItem.MinP != createdRewrite.MinP ||
		rewriteItem.RepeatPenalty != createdRewrite.RepeatPenalty ||
		rewriteItem.MaxTokens != createdRewrite.MaxTokens {
		t.Fatalf("rewrite work item settings = %+v", rewriteItem)
	}
	if err := store.completeRewriteFragment(
		ctx,
		createdRewrite.ID,
		"fragment-2",
		rewriteItem.Fragment.Text,
		"revision-ai",
		RewriterResult{
			RewrittenText: "Второй фрагмент!",
			Reason:        "Уточнена завершающая интонация.",
			ModelID:       createdRewrite.ModelID,
			ModelRevision: "model-revision-1",
			DurationMS:    42,
		},
		now.Add(9*time.Minute+4*time.Second),
	); err != nil {
		t.Fatalf("completeRewriteFragment() error = %v", err)
	}
	if err := store.finishRewriteTask(
		ctx,
		createdRewrite.ID,
		now.Add(9*time.Minute+5*time.Second),
	); err != nil {
		t.Fatalf("finishRewriteTask() error = %v", err)
	}
	storedRewrite, ok, err := store.rewriteTask(ctx, createdRewrite.ID)
	if err != nil || !ok {
		t.Fatalf(
			"rewriteTask() = (%+v, %v, %v), want stored task",
			storedRewrite,
			ok,
			err,
		)
	}
	if storedRewrite.Status != RewriteTaskStatusCompleted ||
		storedRewrite.ModelRevision != "model-revision-1" ||
		storedRewrite.TopK != createdRewrite.TopK ||
		storedRewrite.TopP != createdRewrite.TopP ||
		storedRewrite.MinP != createdRewrite.MinP ||
		storedRewrite.RepeatPenalty != createdRewrite.RepeatPenalty ||
		len(storedRewrite.Fragments) != 1 ||
		!storedRewrite.Fragments[0].Changed ||
		storedRewrite.Fragments[0].RevisionID != "revision-ai" {
		t.Fatalf("stored rewrite snapshot = %+v", storedRewrite)
	}

	edited, err := store.editFragment(
		ctx,
		"fragment-2",
		"Исправленный второй фрагмент.",
		"revision-4",
		now.Add(10*time.Minute),
	)
	if err != nil {
		t.Fatalf("editFragment() error = %v", err)
	}
	if edited.WarningCode != "text_edited" ||
		edited.Text != "Исправленный второй фрагмент." {
		t.Fatalf("editFragment() = %+v, want edited warning", edited)
	}

	retry, err := store.prepareRetry(
		ctx,
		createdJob.ID,
		[]string{"fragment-3", "fragment-2"},
		now.Add(11*time.Minute),
	)
	if err != nil {
		t.Fatalf("prepareRetry(explicit) error = %v", err)
	}
	if !reflect.DeepEqual(
		retry.FragmentIDs,
		[]string{"fragment-3", "fragment-2"},
	) {
		t.Fatalf("explicit retry order = %#v", retry.FragmentIDs)
	}
	if err := store.cancelRetry(ctx, retry, now.Add(12*time.Minute)); err != nil {
		t.Fatalf("cancelRetry() error = %v", err)
	}
	restoredJob, ok, err := store.job(ctx, createdJob.ID)
	if err != nil || !ok || restoredJob.Status != JobStatusFailed {
		t.Fatalf("job after cancelRetry = (%+v, %v, %v)", restoredJob, ok, err)
	}

	retry, err = store.prepareRetry(
		ctx,
		createdJob.ID,
		nil,
		now.Add(13*time.Minute),
	)
	if err != nil {
		t.Fatalf("prepareRetry(all) error = %v", err)
	}
	if !reflect.DeepEqual(
		retry.FragmentIDs,
		[]string{"fragment-2", "fragment-3"},
	) {
		t.Fatalf("all retry order = %#v, want ordinal order", retry.FragmentIDs)
	}
	if err := store.startTask(ctx, retry, now.Add(14*time.Minute)); err != nil {
		t.Fatalf("startTask(retry) error = %v", err)
	}
	for index, fragmentID := range retry.FragmentIDs {
		started, startErr := store.startFragment(
			ctx,
			fragmentID,
			now.Add(time.Duration(15+index)*time.Minute),
		)
		if startErr != nil {
			t.Fatalf("startFragment retry %q error = %v", fragmentID, startErr)
		}
		if started.Attempt != 2 {
			t.Fatalf("retry attempt for %q = %d, want 2", fragmentID, started.Attempt)
		}
		result := testFragmentResult(
			[]byte{byte(index + 2), 1},
			"",
			started.Text,
		)
		if completeErr := store.completeFragment(
			ctx,
			fragmentID,
			result,
			now.Add(time.Duration(17+index)*time.Minute),
		); completeErr != nil {
			t.Fatalf("complete retry %q error = %v", fragmentID, completeErr)
		}
	}
	if err := store.finishTask(ctx, createdJob.ID, now.Add(20*time.Minute)); err != nil {
		t.Fatalf("finishTask(retry) error = %v", err)
	}

	completedJob, ok, err := store.job(ctx, createdJob.ID)
	if err != nil || !ok || completedJob.Status != JobStatusCompleted {
		t.Fatalf("completed job = (%+v, %v, %v)", completedJob, ok, err)
	}
	if completedJob.FragmentsReady != 3 ||
		completedJob.FragmentsPending != 0 ||
		completedJob.FragmentsWarnings != 0 ||
		completedJob.FragmentsFailed != 0 {
		t.Fatalf("completed counters = %+v", completedJob)
	}

	snapshot, err := store.archiveSnapshot(ctx, createdJob.ID)
	if err != nil {
		t.Fatalf("archiveSnapshot() error = %v", err)
	}
	if len(snapshot.Fragments) != 3 {
		t.Fatalf("archive fragments = %d, want 3", len(snapshot.Fragments))
	}
	for index, fragment := range snapshot.Fragments {
		if fragment.Resource.Ordinal != index+1 {
			t.Fatalf(
				"archive ordinal[%d] = %d, want %d",
				index,
				fragment.Resource.Ordinal,
				index+1,
			)
		}
		if len(fragment.AudioPCM) == 0 {
			t.Fatalf("archive fragment %d has empty audio", index)
		}
	}
	if snapshot.Fragments[2].ChapterTitle != "Вторая глава" {
		t.Fatalf(
			"last chapter title = %q, want %q",
			snapshot.Fragments[2].ChapterTitle,
			"Вторая глава",
		)
	}

	catalog, err := store.jobChapterCatalog(ctx, createdJob.ID)
	if err != nil {
		t.Fatalf("jobChapterCatalog() error = %v", err)
	}
	wantCatalog := chapterCatalog{
		BookID: createdBook.ID,
		Chapters: []chapterSummary{
			{
				Number:         1,
				Title:          "Первая глава",
				FragmentsCount: 2,
				DurationMS:     200,
			},
			{
				Number:         2,
				Title:          "Вторая глава",
				FragmentsCount: 1,
				DurationMS:     100,
			},
		},
	}
	if !reflect.DeepEqual(catalog, wantCatalog) {
		t.Errorf("jobChapterCatalog() = %#v, want %#v", catalog, wantCatalog)
	}

	chapterSnapshot, err := store.chapterArchiveSnapshot(
		ctx,
		createdJob.ID,
		2,
	)
	if err != nil {
		t.Fatalf("chapterArchiveSnapshot() error = %v", err)
	}
	if len(chapterSnapshot.Fragments) != 1 ||
		chapterSnapshot.Fragments[0].Resource.ID != "fragment-3" ||
		chapterSnapshot.Fragments[0].Resource.ChapterNumber != 2 ||
		len(chapterSnapshot.Fragments[0].AudioPCM) == 0 {
		t.Errorf(
			"chapterArchiveSnapshot() fragments = %#v",
			chapterSnapshot.Fragments,
		)
	}
	if _, err := store.chapterArchiveSnapshot(
		ctx,
		createdJob.ID,
		99,
	); !errors.Is(err, errChapterNotFound) {
		t.Fatalf(
			"chapterArchiveSnapshot(missing chapter) error = %v, "+
				"want errChapterNotFound",
			err,
		)
	}
	if _, err := store.chapterArchiveSnapshot(
		ctx,
		createdJob.ID,
		0,
	); !errors.Is(err, errInvalid) {
		t.Fatalf(
			"chapterArchiveSnapshot(invalid chapter) error = %v, "+
				"want errInvalid",
			err,
		)
	}

	var attempts int
	if err := store.pool.QueryRow(
		ctx,
		`SELECT count(*) FROM fragment_attempts`,
	).Scan(&attempts); err != nil {
		t.Fatalf("count fragment attempts: %v", err)
	}
	if attempts != 5 {
		t.Fatalf("fragment attempts = %d, want 5 immutable attempts", attempts)
	}

	latestID, ok, err := store.latestJobID(ctx, createdBook.ID)
	if err != nil || !ok || latestID != createdJob.ID {
		t.Fatalf("latestJobID() = (%q, %v, %v)", latestID, ok, err)
	}
	if err := store.deleteJob(ctx, createdJob.ID); err != nil {
		t.Fatalf("deleteJob() error = %v", err)
	}
	if _, ok, err := store.latestJobID(ctx, createdBook.ID); err != nil || ok {
		t.Fatalf("latestJobID() after delete = (_, %v, %v), want absent", ok, err)
	}
	if issues, ok, err := store.jobIssues(ctx, createdJob.ID); err != nil || ok || issues != nil {
		t.Fatalf(
			"jobIssues() after delete = (%#v, %v, %v), want absent",
			issues,
			ok,
			err,
		)
	}
}

func preparePostgresStore(
	t *testing.T,
	ctx context.Context,
	databaseURL string,
) *PostgresStore {
	t.Helper()

	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open integration database: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping integration database: %v", err)
	}

	schema := fmt.Sprintf("book_api_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create integration schema: %v", err)
	}

	migrationFiles, err := filepath.Glob(filepath.Join(
		"..",
		"db",
		"migrations",
		"*.up.sql",
	))
	if err != nil || len(migrationFiles) == 0 {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("list integration migrations: files=%v error=%v", migrationFiles, err)
	}
	sort.Strings(migrationFiles)
	connection, err := admin.Acquire(ctx)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("acquire migration connection: %v", err)
	}
	if _, err := connection.Exec(ctx, "SET search_path TO "+identifier); err != nil {
		connection.Release()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("set migration search path: %v", err)
	}
	for _, migrationFile := range migrationFiles {
		migration, readErr := os.ReadFile(migrationFile)
		if readErr != nil {
			connection.Release()
			_, _ = admin.Exec(
				context.Background(),
				"DROP SCHEMA "+identifier+" CASCADE",
			)
			admin.Close()
			t.Fatalf(
				"read integration migration %q: %v",
				migrationFile,
				readErr,
			)
		}
		if _, execErr := connection.Exec(ctx, string(migration)); execErr != nil {
			connection.Release()
			_, _ = admin.Exec(
				context.Background(),
				"DROP SCHEMA "+identifier+" CASCADE",
			)
			admin.Close()
			t.Fatalf(
				"apply integration migration %q: %v",
				migrationFile,
				execErr,
			)
		}
	}
	connection.Release()

	storeURL, err := url.Parse(databaseURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("parse integration database URL: %v", err)
	}
	query := storeURL.Query()
	query.Set("search_path", schema)
	storeURL.RawQuery = query.Encode()

	store, err := OpenPostgresStore(ctx, storeURL.String())
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("open PostgreSQL store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(
			cleanupCtx,
			"DROP SCHEMA "+identifier+" CASCADE",
		); err != nil {
			t.Errorf("drop integration schema: %v", err)
		}
		admin.Close()
	})

	return store
}

func testFragmentResult(
	audio []byte,
	warningCode string,
	sttText string,
) fragmentResult {
	return fragmentResult{
		AudioPCM:    audio,
		SampleRate:  24_000,
		Channels:    1,
		SampleWidth: 2,
		DurationMS:  100,
		STTText:     sttText,
		STTLanguage: "ru",
		WarningCode: warningCode,
	}
}

func assertPostgresJobState(
	t *testing.T,
	ctx context.Context,
	store *PostgresStore,
	jobID string,
	status JobStatus,
	pending, ready, warnings, failed int,
) {
	t.Helper()

	resource, ok, err := store.job(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("job(%q) = (%+v, %v, %v), want stored job", jobID, resource, ok, err)
	}
	if resource.Status != status ||
		resource.FragmentsPending != pending ||
		resource.FragmentsReady != ready ||
		resource.FragmentsWarnings != warnings ||
		resource.FragmentsFailed != failed {
		t.Fatalf(
			"job(%q) = %+v, want status=%s counts=%d/%d/%d/%d",
			jobID,
			resource,
			status,
			pending,
			ready,
			warnings,
			failed,
		)
	}
}
