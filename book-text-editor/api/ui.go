package api

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html web/chapter.html web/app.css web/app.js web/enhancements.css web/chapter.css web/chapter.js
var uiFileSystem embed.FS

type uiFile struct {
	name        string
	contentType string
	cachePolicy string
	content     []byte
	etag        string
}

var (
	uiIndex   = mustLoadUIFile("web/index.html", "text/html; charset=utf-8", "no-cache")
	uiChapter = mustLoadUIFile("web/chapter.html", "text/html; charset=utf-8", "no-cache")
	uiAssets  = map[string]uiFile{
		"app.css":          mustLoadUIFile("web/app.css", "text/css; charset=utf-8", "no-cache"),
		"enhancements.css": mustLoadUIFile("web/enhancements.css", "text/css; charset=utf-8", "no-cache"),
		"app.js":           mustLoadUIFile("web/app.js", "text/javascript; charset=utf-8", "no-cache"),
		"chapter.css":      mustLoadUIFile("web/chapter.css", "text/css; charset=utf-8", "no-cache"),
		"chapter.js":       mustLoadUIFile("web/chapter.js", "text/javascript; charset=utf-8", "no-cache"),
	}
)

func registerUIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", serveUIIndex)
	mux.HandleFunc("GET /jobs/{jobID}/chapters/{chapterNumber}", serveUIChapter)
	mux.HandleFunc("GET /assets/{name}", serveUIAsset)
}

func serveUIIndex(writer http.ResponseWriter, request *http.Request) {
	serveUIFile(writer, request, uiIndex)
}

func serveUIChapter(writer http.ResponseWriter, request *http.Request) {
	if _, err := positiveChapterNumber(request.PathValue("chapterNumber")); err != nil ||
		strings.TrimSpace(request.PathValue("jobID")) == "" {
		http.NotFound(writer, request)
		return
	}
	serveUIFile(writer, request, uiChapter)
}

func serveUIAsset(writer http.ResponseWriter, request *http.Request) {
	file, ok := uiAssets[request.PathValue("name")]
	if !ok {
		http.NotFound(writer, request)
		return
	}
	serveUIFile(writer, request, file)
}

func serveUIFile(writer http.ResponseWriter, request *http.Request, file uiFile) {
	setUISecurityHeaders(writer.Header())
	writer.Header().Set("Content-Type", file.contentType)
	writer.Header().Set("Cache-Control", file.cachePolicy)
	writer.Header().Set("ETag", file.etag)
	if requestETagMatches(request.Header.Get("If-None-Match"), file.etag) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeContent(writer, request, file.name, time.Time{}, bytes.NewReader(file.content))
}

func requestETagMatches(headerValue, current string) bool {
	current = strings.TrimPrefix(current, "W/")
	for value := range strings.SplitSeq(headerValue, ",") {
		value = strings.TrimSpace(value)
		if value == "*" || strings.TrimPrefix(value, "W/") == current {
			return true
		}
	}
	return false
}

func setUISecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; object-src 'none'; "+
			"frame-ancestors 'none'; form-action 'self'; "+
			"script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"media-src 'self'; connect-src 'self'")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("X-Content-Type-Options", "nosniff")
}

func mustLoadUIFile(name, contentType, cachePolicy string) uiFile {
	content, err := uiFileSystem.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("load embedded UI file %q: %v", name, err))
	}
	digest := sha256.Sum256(content)
	return uiFile{
		name:        name,
		contentType: contentType,
		cachePolicy: cachePolicy,
		content:     content,
		etag:        `"` + hex.EncodeToString(digest[:]) + `"`,
	}
}
