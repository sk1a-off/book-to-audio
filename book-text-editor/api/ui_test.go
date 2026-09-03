package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIServesDashboardAndChapterPages(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	tests := []struct {
		path        string
		contentType string
		contains    []string
	}{
		{
			path: "/", contentType: "text/html; charset=utf-8",
			contains: []string{
				`<html lang="ru">`, `id="book-upload-form"`, `id="job-dashboard"`,
				`id="chapter-downloads"`, `src="/assets/app.js"`,
				`id="queue-dialog"`, `id="queue-jobs-list"`,
				`id="job-pause-button"`, `id="job-continue-button"`,
				`id="pronunciation-enabled"`, `id="pronunciation-rules"`,
				`id="russian-selective-stress" type="checkbox" checked`,
				`id="russian-normalize-morphology" type="checkbox" checked`,
				`Контекстные ударения для проверенных омографов`,
				`Массовая разметка каждого слова ухудшает интонацию`,
				`id="quality-preview-text"`, `id="quality-preview-button"`,
				`id="quality-preview-audio"`, `data-state="idle"`,
				`id="rewrite-all-button"`, `Исправить все warnings`,
				`.fb2.zip`,
			},
		},
		{
			path: "/assets/app.css", contentType: "text/css; charset=utf-8",
			contains: []string{
				".queue-dialog", ".queue-window-body", ".queue-fragment-card",
				".job-topline-actions", `.status-badge[data-status="paused"]`,
				"@media (max-width: 760px)",
			},
		},
		{
			path: "/jobs/job-test/chapters/1", contentType: "text/html; charset=utf-8",
			contains: []string{
				`id="chapter-title"`, `id="fragment-list"`, `id="status-filter"`,
				`id="rewrite-panel"`, `id="rewrite-model-select"`,
				`id="rewrite-selected-button"`, `id="rewrite-progress-region"`,
				`ниже 92%`,
				`href="/assets/chapter.css"`, `src="/assets/chapter.js"`,
			},
		},
		{
			path: "/assets/chapter.css", contentType: "text/css; charset=utf-8",
			contains: []string{
				".fragment-card", ".pagination", ".rewrite-panel",
				".rewrite-progress", ".rewrite-fragment-selection",
				"prefers-reduced-motion",
			},
		},
		{
			path: "/assets/chapter.js", contentType: "text/javascript; charset=utf-8",
			contains: []string{
				"class ChapterPage", "PAGE_SIZE = 20", "credentials: \"same-origin\"",
				"Повторить только этот фрагмент", "transcript_score", "encodeURIComponent",
				"rewriteModels()", "rewriteWarnings(jobID, payload)",
				"pollRewrite(rewriteID)", "REWRITE_STORAGE_KEY",
				"Переписать выбранные", `toFixed(1)`, "snapshotRenderSignature",
				"fragmentRenderSignature",
			},
		},
		{
			path: "/assets/app.js", contentType: "text/javascript; charset=utf-8",
			contains: []string{
				"class APIClient", "automatic_warning_retries: 1",
				"Math.min(candidate, 1)",
				"deleteVoice(voiceID)", "deleteJob(jobID)",
				"getQueue()", "pauseJob(jobID)", "resumeJob(jobID)",
				"cancelJob(jobID)", "startQueueMonitor()", "loadWarningOverview(jobID",
				`fragment_ids: selectedIDs`,
				"Удалить задачу", "Удалить голос", "window.confirm",
				"jobsRenderSignature", "queueRenderSignature",
				"currentJobRenderSignature", "validatePronunciationRules",
				`pronunciation: Object.freeze`, "generateQualityPreview",
				`russian_text: Object.freeze`, "russianSelectiveStress",
				`RUSSIAN_TEXT_DEFAULTS_REVISION`, `selective_stress: true`,
				"URL.revokeObjectURL", "DEFAULT_QUALITY_PREVIEW_TEXT",
				"invalidateQualityPreview", "qualityPreviewRevision",
				`field === this.nodes.pronunciationRules`, `? "input"`,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			response := endpointTestRequest(t, handler, http.MethodGet, test.path, nil, "")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d body=%s", test.path, response.Code, response.Body)
			}
			if got := response.Header().Get("Content-Type"); got != test.contentType {
				t.Errorf("GET %s Content-Type=%q", test.path, got)
			}
			if response.Header().Get("ETag") == "" {
				t.Errorf("GET %s has no ETag", test.path)
			}
			endpointTestAssertUISecurityHeaders(t, response)
			body := response.Body.String()
			for _, expected := range test.contains {
				if !strings.Contains(body, expected) {
					t.Errorf("GET %s body misses %q", test.path, expected)
				}
			}
		})
	}
}

func TestUIJavaScriptUsesSafeSameOriginDOMRendering(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	var javascript strings.Builder
	for _, path := range []string{"/assets/app.js", "/assets/chapter.js"} {
		response := endpointTestRequest(t, handler, http.MethodGet, path, nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d", path, response.Code)
		}
		javascript.WriteString(response.Body.String())
		javascript.WriteByte('\n')
	}
	content := javascript.String()
	for _, forbidden := range []string{
		"innerHTML", "outerHTML", ".style.", "http://", "https://", "localhost:", "127.0.0.1:",
	} {
		if strings.Contains(content, forbidden) {
			t.Errorf("JavaScript contains unsafe/direct reference %q", forbidden)
		}
	}
	for _, required := range []string{
		"credentials: \"same-origin\"", "encodeURIComponent", "JSON.stringify", "textContent",
		"window.localStorage", "REWRITE_TERMINAL_STATUSES",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("JavaScript misses guard %q", required)
		}
	}
}

func TestUIDashboardExposesDurableMultiBookQueueControls(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	page := endpointTestRequest(t, handler, http.MethodGet, "/", nil, "")
	javascript := endpointTestRequest(t, handler, http.MethodGet, "/assets/app.js", nil, "")
	if page.Code != http.StatusOK || javascript.Code != http.StatusOK {
		t.Fatalf("UI assets status: page=%d js=%d", page.Code, javascript.Code)
	}
	pageContent := page.Body.String()
	for _, required := range []string{
		`id="queue-dialog"`, `id="queue-open-button"`,
		`id="job-pause-button"`, `id="job-continue-button"`,
		`id="rewrite-all-button"`, `Поставить книгу в очередь`,
	} {
		if !strings.Contains(pageContent, required) {
			t.Errorf("dashboard misses %q", required)
		}
	}

	javascriptContent := javascript.Body.String()
	for _, required := range []string{
		`this.request("/v1/queue")`, `/pause`, `/resume`, `/cancel`,
		"startQueueMonitor", "loadWarningOverview", "fragment_ids: selectedIDs",
	} {
		if !strings.Contains(javascriptContent, required) {
			t.Errorf("dashboard JavaScript misses %q", required)
		}
	}
	if strings.Contains(
		javascriptContent,
		"Дождитесь завершения текущей задачи",
	) {
		t.Error("dashboard still blocks starting a second book")
	}
}

func TestUIRoutesDoNotShadowAPI(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	health := endpointTestRequest(t, handler, http.MethodGet, "/healthz", nil, "")
	if health.Code != http.StatusOK || strings.Contains(health.Body.String(), "<!doctype html>") {
		t.Fatalf("GET /healthz shadowed: status=%d body=%q", health.Code, health.Body.String())
	}
	for _, path := range []string{
		"/missing", "/v1/missing", "/assets/missing.js", "/jobs/job/chapters/not-a-number",
	} {
		response := endpointTestRequest(t, handler, http.MethodGet, path, nil, "")
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s status=%d, want 404", path, response.Code)
		}
	}
}

func TestUIConditionalAndHeadRequests(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	initial := endpointTestRequest(t, handler, http.MethodGet, "/assets/chapter.js", nil, "")
	etag := initial.Header().Get("ETag")
	request := httptest.NewRequest(http.MethodGet, "/assets/chapter.js", nil)
	request.Header.Set("If-None-Match", etag)
	conditional := endpointTestDo(t, handler, request)
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Errorf("conditional GET status=%d body=%d", conditional.Code, conditional.Body.Len())
	}
	head := endpointTestRequest(t, handler, http.MethodHead, "/", nil, "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD / status=%d body=%d", head.Code, head.Body.Len())
	}
}

func endpointTestAssertUISecurityHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	csp := response.Header().Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'", "script-src 'self'", "style-src 'self'",
		"media-src 'self' blob:", "connect-src 'self'", "frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP %q misses %q", csp, directive)
		}
	}
	if strings.Contains(csp, "'unsafe-inline'") {
		t.Errorf("CSP permits inline content: %q", csp)
	}
	for name, want := range map[string]string{
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}
}
