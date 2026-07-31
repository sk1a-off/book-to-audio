package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIServesEmbeddedApplicationAndAssets(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()

	tests := []struct {
		path        string
		contentType string
		contains    []string
	}{
		{
			path:        "/",
			contentType: "text/html; charset=utf-8",
			contains: []string{
				`<html lang="ru">`,
				"Голос Книги",
				`id="book-upload-form"`,
				`id="voice-upload-form"`,
				`id="generate-button"`,
				`id="generation-settings"`,
				`id="tts-num-steps"`,
				`id="tts-guidance-scale"`,
				`id="tts-t-shift"`,
				`id="stt-beam-size"`,
				`id="warning-retries"`,
				`id="generation-settings-reset"`,
				`id="jobs-status-filter"`,
				`id="jobs-list"`,
				`id="jobs-prev-button"`,
				`id="jobs-next-button"`,
				`id="warnings-section"`,
				`id="rewrite-panel"`,
				`id="rewrite-model-select"`,
				`id="rewrite-prompt"`,
				`id="rewrite-temperature"`,
				`id="rewrite-top-k"`,
				`id="rewrite-top-p"`,
				`id="rewrite-min-p"`,
				`id="rewrite-repeat-penalty"`,
				`id="rewrite-max-tokens"`,
				`id="rewrite-selected-button"`,
				`id="rewrite-all-button"`,
				`id="rewrite-progress-region"`,
				`id="job-settings-snapshot"`,
				`id="chapter-downloads"`,
				"готовые главы FLAC",
				"ZIP с главами FLAC",
				"Вся книга · главы FLAC, ZIP",
				"Готовые главы FLAC",
				`href="/assets/app.css"`,
				`src="/assets/app.js"`,
			},
		},
		{
			path:        "/assets/app.css",
			contentType: "text/css; charset=utf-8",
			contains: []string{
				":root",
				".materials-grid",
				".jobs-monitor",
				".jobs-card",
				".generation-settings",
				".job-settings-snapshot",
				".warning-card",
				".manual-review",
				".manual-review--text-only",
				".revision-history",
				".rewrite-panel",
				".rewrite-progress",
				"prefers-reduced-motion",
				"forced-colors",
			},
		},
		{
			path:        "/assets/app.js",
			contentType: "text/javascript; charset=utf-8",
			contains: []string{
				`this.request("/v1/book"`,
				`this.request("/v1/voices"`,
				"`/v1/generate/book/${encodeURIComponent(bookID)}`",
				"body: JSON.stringify(settings)",
				"`/v1/jobs?${parameters.toString()}`",
				"`/v1/job/${encodeURIComponent(jobID)}/warnings`",
				"`/v1/fragment/${encodeURIComponent(fragmentID)}`",
				"`/v1/job/${encodeURIComponent(jobID)}/retry/warnings`",
				"`/v1/fragment/${encodeURIComponent(fragmentID)}/approve`",
				"`/v1/fragment/${encodeURIComponent(fragmentID)}/revisions`",
				"`${encodeURIComponent(revisionID)}/restore`",
				`this.request("/v1/rewrite/models")`,
				"`/v1/job/${encodeURIComponent(jobID)}/rewrite/warnings`",
				"`/v1/rewrite/${encodeURIComponent(rewriteID)}`",
				"`/v1/fragment/${encodeURIComponent(fragment.id)}/audio.wav`",
				"`/v1/job/${encodeURIComponent(job.id)}/audio.zip`",
				"`/v1/job/${encodeURIComponent(jobID)}/chapters`",
				"`${encodeURIComponent(String(chapter.chapter_number))}/audio.zip`",
				`book-${job.book_id || "audio"}-chapters-flac.zip`,
				"ready-chapter-flac-",
				"chapterFLACFilename",
				"chapter.audio_filename",
				"один FLAC в ZIP",
				"character_",
				"textContent",
				"URLSearchParams",
				"window.localStorage",
				"DEFAULT_GENERATION_SETTINGS",
				"collectGenerationSettings",
				"field.input.checkValidity()",
				"audiobook-ui.generation-settings.v1",
				"audiobook-ui.active-rewrite.v1",
				"persistActiveRewrite",
				"clearStoredRewrite",
				"TEXT_ONLY_WARNING_CODES",
				"top_k: topK",
				"repeat_penalty: repeatPenalty",
				`audio.preload = "none"`,
				"audio.controls = true",
				"REWRITE_TERMINAL_STATUSES",
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.path, func(t *testing.T) {
			response := endpointTestRequest(
				t,
				handler,
				http.MethodGet,
				test.path,
				nil,
				"",
			)
			if response.Code != http.StatusOK {
				t.Fatalf(
					"GET %s status = %d, body = %s",
					test.path,
					response.Code,
					response.Body,
				)
			}
			if got := response.Header().Get("Content-Type"); got !=
				test.contentType {
				t.Errorf("GET %s Content-Type = %q", test.path, got)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("GET %s Cache-Control = %q", test.path, got)
			}
			if etag := response.Header().Get("ETag"); etag == "" {
				t.Errorf("GET %s has no ETag", test.path)
			}
			endpointTestAssertUISecurityHeaders(t, response)

			body := response.Body.String()
			for _, expected := range test.contains {
				if !strings.Contains(body, expected) {
					t.Errorf("GET %s body does not contain %q", test.path, expected)
				}
			}
		})
	}
}

func TestUIUsesOnlyPublicCanonicalAPIAndSafeDOMRendering(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()

	htmlResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/",
		nil,
		"",
	)
	html := htmlResponse.Body.String()
	for _, forbidden := range []string{
		" onclick=",
		" onsubmit=",
		" style=",
		"<script>",
		"zip с wav",
	} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Errorf("HTML contains forbidden inline content %q", forbidden)
		}
	}

	jsResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/assets/app.js",
		nil,
		"",
	)
	javascript := jsResponse.Body.String()
	for _, forbidden := range []string{
		"innerHTML",
		"outerHTML",
		".style.",
		"/v1/job/warnings/",
		"/v1/job/retry/warnings",
		"/v1/book/${",
		"OMNIVOICE",
		"STT_URL",
		"localhost:",
		"127.0.0.1:",
		"http://",
		"https://",
	} {
		if strings.Contains(javascript, forbidden) {
			t.Errorf("JavaScript contains forbidden direct/unsafe reference %q", forbidden)
		}
	}
	for _, required := range []string{
		"busyOperations.has",
		"TERMINAL_JOB_STATUSES",
		"jobsRefreshInFlight",
		"JOBS_POLL_INTERVAL_MS",
		`job.status === "completed"`,
		"encodeURIComponent",
		"JSON.stringify",
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("JavaScript does not contain lifecycle guard %q", required)
		}
	}
}

func TestUIRouteIsExactAndDoesNotShadowAPIOrUnknownPaths(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()

	health := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/healthz",
		nil,
		"",
	)
	if health.Code != http.StatusOK ||
		!strings.HasPrefix(
			health.Header().Get("Content-Type"),
			"application/json",
		) ||
		strings.Contains(health.Body.String(), "<!doctype html>") {
		t.Errorf(
			"GET /healthz was shadowed: status=%d content-type=%q body=%q",
			health.Code,
			health.Header().Get("Content-Type"),
			health.Body.String(),
		)
	}

	for _, path := range []string{
		"/does-not-exist",
		"/v1/does-not-exist",
		"/assets/missing.js",
		"/assets/app.js/extra",
	} {
		response := endpointTestRequest(
			t,
			handler,
			http.MethodGet,
			path,
			nil,
			"",
		)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, response.Code)
		}
		if strings.Contains(response.Body.String(), "<!doctype html>") {
			t.Errorf("GET %s unexpectedly returned the UI shell", path)
		}
	}

	methodNotAllowed := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/",
		nil,
		"",
	)
	if methodNotAllowed.Code != http.StatusMethodNotAllowed {
		t.Errorf(
			"POST / status = %d, want %d",
			methodNotAllowed.Code,
			http.StatusMethodNotAllowed,
		)
	}
	if allow := methodNotAllowed.Header().Get("Allow"); !strings.Contains(
		allow,
		http.MethodGet,
	) {
		t.Errorf("POST / Allow = %q", allow)
	}
}

func TestUIConditionalAndHeadRequests(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()

	initial := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/assets/app.js",
		nil,
		"",
	)
	etag := initial.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET app.js has no ETag")
	}

	conditionalRequest := httptest.NewRequest(
		http.MethodGet,
		"/assets/app.js",
		nil,
	)
	conditionalRequest.Header.Set(
		"If-None-Match",
		`"unrelated", W/`+etag,
	)
	conditional := endpointTestDo(t, handler, conditionalRequest)
	if conditional.Code != http.StatusNotModified {
		t.Errorf(
			"conditional GET status = %d, want %d",
			conditional.Code,
			http.StatusNotModified,
		)
	}
	if conditional.Body.Len() != 0 {
		t.Errorf("conditional GET body length = %d", conditional.Body.Len())
	}

	head := endpointTestRequest(
		t,
		handler,
		http.MethodHead,
		"/",
		nil,
		"",
	)
	if head.Code != http.StatusOK {
		t.Errorf("HEAD / status = %d", head.Code)
	}
	if head.Body.Len() != 0 {
		t.Errorf("HEAD / body length = %d", head.Body.Len())
	}
	if head.Header().Get("Content-Length") == "" {
		t.Error("HEAD / has no Content-Length")
	}
}

func endpointTestAssertUISecurityHeaders(
	t *testing.T,
	response *httptest.ResponseRecorder,
) {
	t.Helper()

	csp := response.Header().Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"connect-src 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("Content-Security-Policy %q misses %q", csp, directive)
		}
	}
	if strings.Contains(csp, "'unsafe-inline'") {
		t.Errorf("Content-Security-Policy permits inline content: %q", csp)
	}

	expected := map[string]string{
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
	}
	for name, want := range expected {
		if got := response.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
