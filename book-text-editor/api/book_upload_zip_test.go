package api

import (
	"archive/zip"
	"bytes"
	"net/http"
	"testing"
)

func TestUploadBookAcceptsFB2ZIP(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("book.fb2")
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	if _, err := entry.Write([]byte(endpointTestFB2)); err != nil {
		t.Fatalf("Write(): %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	response := endpointTestDo(t, fixture.server.Handler(), endpointTestMultipartRequest(
		t, http.MethodPost, "/v1/book", "file", "book.fb2.zip", archive.Bytes(), nil,
	))
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", response.Code, response.Body)
	}
	var book BookResource
	endpointTestDecodeJSON(t, response, &book)
	if book.ChaptersCount != 1 || book.FragmentsCount != 2 {
		t.Fatalf("book=%+v", book)
	}
}
