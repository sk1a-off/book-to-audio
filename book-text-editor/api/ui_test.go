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
				`id="book-upload-form"`,
				`id="voice-upload-form"`,
				`id="generate-button"`,
				`id="job-dashboard"`,
				`id="chapter-downloads"`,
				`id="fragment-studio"`,
				`id="fragment-studio-list"`,
				`id="warnings-list"`,
				`href="/assets/app.css"`,
				`href="/assets/enhancements.css"`,
				`src="/assets/app.js"`,
				`src="/assets/fragments.js"`,
			},
		},
		{
			path:        "/assets/app.css",
			contentType: "text/css; charset=utf-8",
			contains: []string{
				":root",
				".warning-card",
				".chapter-downloads",
				"prefers-reduced-motion",
			},
		},
		{
			path:        "/assets/enhancements.css",
			contentType: "text/css; charset=utf-8",
			contains: []string{
				".fragment-studio",
				".fragment-counter-grid",
				".fragment-card-body",
				".fragment-status[data-status=\"warning\"]",
				"prefers-reduced-motion",
			},
		},
		{
			path:        "/assets/app.js",
			contentType: "text/javascript; charset=utf-8",
			contains: []string{
				"class APIClient",
				"uploadBook(file)",
				"DEFAULT_GENERATION_SETTINGS",
				"getFragmentRevisions(fragmentID)",
				"restoreFragmentRevision(fragmentID, revisionID, reason)",
				"rewriteWarnings(jobID, payload)",
				"resumeStoredRewrite()",
				"renderRewriteTask(task)",
				"audio.preload = \"none\"",
				"window.localStorage",
			},
		},
		{
			path:        "/assets/fragments.js",
			contentType: "text/javascript; charset=utf-8",
			contains: []string{
				"class FragmentStudio",
				"catalog(jobID)",
				"edit(fragmentID, newText)",
				"Сохранить и переозвучить",
				"fragment.audio_available",
				"audio.preload = \"none\"",
				"/audio.flac",
				"FRAGMENT_SAVED_QUEUE_FULL",
				"MutationObserver",
				"encodeURIComponent",
				"JSON.stringify",
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
				t.Fatalf("GET %s status = %d, body = %s", test.path, response.Code, response.Body)
			}
			if got := response.Header().Get("Content-Type"); got != test.contentType {
				t.Errorf("GET %s Content-Type = %q", test.path, got)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("GET %s Cache-Control = %q", test.path, got)
			}
			if response.Header().Get("ETag") == "" {
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

func TestUIUsesSafeSameOriginDOMRendering(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	var javascript strings.Builder
	for _, path := range []string{"/assets/app.js", "/assets/fragments.js"} {
		response := endpointTestRequest(
			t,
			handler,
			http.MethodGet,
			path,
			nil,
			"",
		)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, response.Code)
		}
		javascript.WriteString(response.Body.String())
		javascript.WriteByte('\n')
	}
	content := javascript.String()
	for _, forbidden := range []string{
		"innerHTML",
		"outerHTML",
		".style.",
		"http://",
		"https://",
		"localhost:",
		"127.0.0.1:",
	} {
		if strings.Contains(content, forbidden) {
			t.Errorf("JavaScript contains unsafe/direct reference %q", forbidden)
		}
	}
	for _, required := range []string{
		"credentials: \"same-origin\"",
		"encodeURIComponent",
		"JSON.stringify",
		"busyOperations.has",
		"TERMINAL_JOB_STATUSES",
		"textContent",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("JavaScript misses lifecycle guard %q", required)
		}
	}
}

func TestUIRouteIsExactAndDoesNotShadowAPI(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()

	health := endpointTestRequest(t, handler, http.MethodGet, "/healthz", nil, "")
	if health.Code != http.StatusOK || strings.Contains(health.Body.String(), "<!doctype html>") {
		t.Fatalf("GET /healthz was shadowed: status=%d body=%q", health.Code, health.Body.String())
	}
	for _, path := range []string{"/missing", "/v1/missing", "/assets/missing.js"} {
		response := endpointTestRequest(t, handler, http.MethodGet, path, nil, "")
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, response.Code)
		}
	}
}

func TestUIConditionalAndHeadRequests(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	handler := fixture.server.Handler()
	initial := endpointTestRequest(t, handler, http.MethodGet, "/assets/fragments.js", nil, "")
	etag := initial.Header().Get("ETag")
	request := httptest.NewRequest(http.MethodGet, "/assets/fragments.js", nil)
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
		"media-src 'self'",
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
	for name, want := range map[string]string{
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
