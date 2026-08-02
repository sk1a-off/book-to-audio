package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestParseJobListFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		rawQuery     string
		wantStatuses []JobStatus
		wantLimit    int
		wantOffset   int
		wantError    bool
	}{
		{
			name:       "defaults",
			wantLimit:  defaultJobsPageSize,
			wantOffset: 0,
		},
		{
			name:     "active jobs",
			rawQuery: "status=active&limit=25&offset=50",
			wantStatuses: []JobStatus{
				JobStatusQueued,
				JobStatusRunning,
				JobStatusPaused,
			},
			wantLimit:  25,
			wantOffset: 50,
		},
		{
			name:         "exact status",
			rawQuery:     "status=completed_with_warnings&limit=100",
			wantStatuses: []JobStatus{JobStatusCompletedWithWarnings},
			wantLimit:    maxJobsPageSize,
			wantOffset:   0,
		},
		{
			name:       "explicit all",
			rawQuery:   "status=all",
			wantLimit:  defaultJobsPageSize,
			wantOffset: 0,
		},
		{name: "unknown status", rawQuery: "status=waiting", wantError: true},
		{name: "empty status", rawQuery: "status=", wantError: true},
		{name: "duplicate status", rawQuery: "status=all&status=active", wantError: true},
		{name: "zero limit", rawQuery: "limit=0", wantError: true},
		{name: "oversized limit", rawQuery: "limit=101", wantError: true},
		{name: "signed limit", rawQuery: "limit=%2B1", wantError: true},
		{name: "negative offset", rawQuery: "offset=-1", wantError: true},
		{name: "overflowing offset", rawQuery: "offset=999999999999999999999999", wantError: true},
		{name: "duplicate offset", rawQuery: "offset=0&offset=1", wantError: true},
		{name: "unknown parameter", rawQuery: "sort=created_at", wantError: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			query, err := url.ParseQuery(test.rawQuery)
			if err != nil {
				t.Fatalf("url.ParseQuery() error = %v", err)
			}
			filter, err := parseJobListFilter(query)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseJobListFilter() = %+v, want error", filter)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJobListFilter() error = %v", err)
			}
			if !reflect.DeepEqual(filter.Statuses, test.wantStatuses) ||
				filter.Limit != test.wantLimit ||
				filter.Offset != test.wantOffset {
				t.Fatalf(
					"parseJobListFilter() = %+v, want statuses=%v limit=%d offset=%d",
					filter,
					test.wantStatuses,
					test.wantLimit,
					test.wantOffset,
				)
			}
		})
	}
}

func TestMemoryStoreJobsFiltersSortsAndPaginates(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	for _, resource := range []JobResource{
		testListedJob("old-running", JobStatusRunning, now.Add(-time.Minute)),
		testListedJob("same-a", JobStatusFailed, now),
		testListedJob("same-z", JobStatusQueued, now),
		testListedJob("new-completed", JobStatusCompleted, now.Add(time.Minute)),
	} {
		store.jobs[resource.ID] = &jobRecord{Resource: resource}
	}

	page, err := store.listJobs(context.Background(), jobListFilter{
		Limit:  2,
		Offset: 1,
	})
	if err != nil {
		t.Fatalf("jobs(all) error = %v", err)
	}
	assertListedJobs(t, page, 4, "same-z", "same-a")

	page, err = store.listJobs(context.Background(), jobListFilter{
		Statuses: []JobStatus{JobStatusQueued, JobStatusRunning},
		Limit:    maxJobsPageSize,
	})
	if err != nil {
		t.Fatalf("jobs(active) error = %v", err)
	}
	assertListedJobs(t, page, 2, "same-z", "old-running")

	page, err = store.listJobs(context.Background(), jobListFilter{
		Statuses: []JobStatus{JobStatusFailed},
		Limit:    maxJobsPageSize,
	})
	if err != nil {
		t.Fatalf("jobs(failed) error = %v", err)
	}
	assertListedJobs(t, page, 1, "same-a")

	page, err = store.listJobs(context.Background(), jobListFilter{
		Limit:  1,
		Offset: 100,
	})
	if err != nil {
		t.Fatalf("jobs(beyond last page) error = %v", err)
	}
	if page.Total != 4 || page.Jobs == nil || len(page.Jobs) != 0 {
		t.Fatalf("jobs(beyond last page) = %#v, want non-nil empty page", page)
	}

	_, err = store.listJobs(context.Background(), jobListFilter{
		Statuses: []JobStatus{"not-a-status"},
		Limit:    1,
	})
	if !errors.Is(err, errInvalid) {
		t.Fatalf("jobs(invalid filter) error = %v, want errInvalid", err)
	}
}

func TestListJobsEndpoint(t *testing.T) {
	t.Parallel()

	store := newMemoryStore()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	for _, resource := range []JobResource{
		testListedJob("running", JobStatusRunning, now),
		testListedJob("queued", JobStatusQueued, now.Add(-time.Second)),
		testListedJob("completed", JobStatusCompleted, now.Add(time.Second)),
	} {
		store.jobs[resource.ID] = &jobRecord{Resource: resource}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &Server{store: store, logger: logger}
	handler := requestMiddleware(logger, http.HandlerFunc(server.listJobs))

	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodGet,
			"/v1/jobs?status=active&limit=1&offset=1",
			nil,
		),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /v1/jobs status = %d, body = %s", response.Code, response.Body)
	}
	var page JobsResponse
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("decode jobs response: %v", err)
	}
	if page.Total != 2 || page.Limit != 1 || page.Offset != 1 ||
		len(page.Jobs) != 1 || page.Jobs[0].ID != "queued" {
		t.Fatalf("GET /v1/jobs response = %+v", page)
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(
		invalid,
		httptest.NewRequest(http.MethodGet, "/v1/jobs?limit=101", nil),
	)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf(
			"GET invalid /v1/jobs status = %d, body = %s",
			invalid.Code,
			invalid.Body,
		)
	}
	var problem ErrorResponse
	if err := json.NewDecoder(invalid.Body).Decode(&problem); err != nil {
		t.Fatalf("decode invalid query response: %v", err)
	}
	if problem.Code != "INVALID_QUERY" || problem.RequestID == "" {
		t.Fatalf("invalid query response = %+v", problem)
	}
}

func TestPostgresStoreJobsIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := preparePostgresStore(t, ctx, databaseURL)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

	_, err := store.pool.Exec(
		ctx,
		`INSERT INTO books (
			id, title, authors, format, chapters_count, fragments_count,
			status, created_at
		) VALUES ('book-list', 'List test', '{}', 'fb2', 0, 0, 'ready', $1)`,
		now,
	)
	if err != nil {
		t.Fatalf("insert list test book: %v", err)
	}
	_, err = store.pool.Exec(
		ctx,
		`INSERT INTO voices (
			id, name, mode, format, content_type, size_bytes,
			has_reference_text, status, reference_text, reference_audio,
			created_at
		) VALUES (
			'voice-list', 'List voice', 'voice_clone', 'wav', 'audio/wav',
			0, false, 'ready', '', ''::bytea, $1
		)`,
		now,
	)
	if err != nil {
		t.Fatalf("insert list test voice: %v", err)
	}

	for _, resource := range []JobResource{
		testListedJob("pg-old-running", JobStatusRunning, now.Add(-time.Minute)),
		testListedJob("pg-same-a", JobStatusFailed, now),
		testListedJob("pg-same-z", JobStatusQueued, now),
		testListedJob("pg-new-completed", JobStatusCompleted, now.Add(time.Minute)),
	} {
		_, err = store.pool.Exec(
			ctx,
			`INSERT INTO jobs (
				id, book_id, voice_id, status, fragments_count,
				fragments_pending, fragments_ready, fragments_warnings,
				fragments_failed, created_at, updated_at
			) VALUES ($1, 'book-list', 'voice-list', $2, 0, 0, 0, 0, 0, $3, $3)`,
			resource.ID,
			resource.Status,
			resource.CreatedAt,
		)
		if err != nil {
			t.Fatalf("insert list test job %q: %v", resource.ID, err)
		}
	}

	page, err := store.listJobs(ctx, jobListFilter{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("PostgresStore.jobs(all) error = %v", err)
	}
	assertListedJobs(t, page, 4, "pg-same-z", "pg-same-a")

	page, err = store.listJobs(ctx, jobListFilter{
		Statuses: []JobStatus{JobStatusQueued, JobStatusRunning},
		Limit:    maxJobsPageSize,
	})
	if err != nil {
		t.Fatalf("PostgresStore.jobs(active) error = %v", err)
	}
	assertListedJobs(t, page, 2, "pg-same-z", "pg-old-running")
}

func testListedJob(id string, status JobStatus, createdAt time.Time) JobResource {
	return JobResource{
		ID:        id,
		BookID:    "book-list",
		VoiceID:   "voice-list",
		Status:    status,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

func assertListedJobs(
	t *testing.T,
	page jobListPage,
	wantTotal int64,
	wantIDs ...string,
) {
	t.Helper()

	if page.Total != wantTotal {
		t.Fatalf("jobs total = %d, want %d", page.Total, wantTotal)
	}
	gotIDs := make([]string, 0, len(page.Jobs))
	for _, resource := range page.Jobs {
		gotIDs = append(gotIDs, resource.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("jobs IDs = %#v, want %#v", gotIDs, wantIDs)
	}
}
