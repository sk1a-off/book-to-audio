package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"book-text-editor/internal/book"
	"book-text-editor/internal/bookinput"
	"book-text-editor/internal/fb2"
)

func parseBookUpload(
	reader io.Reader,
	filename string,
	parser fb2.Parser,
	limit int64,
) (book.Book, error) {
	data, _, err := bookinput.Read(reader, filename, limit)
	if err != nil {
		return book.Book{}, err
	}
	parsed, err := parser.Parse(bytes.NewReader(data))
	if err != nil {
		return book.Book{}, fmt.Errorf("parse FB2: %w", err)
	}
	return parsed, nil
}

// Keep explicit error classification near the transport adapter.
func bookUploadProblem(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, bookinput.ErrTooLarge):
		return 413, "BOOK_TOO_LARGE", "FB2 document exceeds the upload limit after unpacking"
	case errors.Is(err, bookinput.ErrNoFB2):
		return 422, "FB2_NOT_FOUND_IN_ZIP", "ZIP archive does not contain an FB2 document"
	case errors.Is(err, bookinput.ErrAmbiguousFB2):
		return 422, "AMBIGUOUS_FB2_ZIP", "ZIP archive contains multiple FB2 documents and no unique book can be selected"
	case errors.Is(err, bookinput.ErrInvalidZIP):
		return 422, "INVALID_FB2_ZIP", "uploaded ZIP archive is not readable"
	default:
		return 422, "INVALID_FB2", "uploaded file is not a readable FB2 document"
	}
}

// Dummy private aliases are intentionally absent: uploadBook calls
// bookUploadProblem directly using net/http types from endpoints.go.
