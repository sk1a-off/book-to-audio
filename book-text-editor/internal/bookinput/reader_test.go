package bookinput

import (
	"archive/zip"
	"bytes"
	"errors"
	"testing"
)

func TestReadPlainAndZippedFB2(t *testing.T) {
	t.Parallel()
	plain := []byte("<FictionBook>plain</FictionBook>")
	got, name, err := Read(bytes.NewReader(plain), "book.fb2", 1024)
	if err != nil || !bytes.Equal(got, plain) || name != "book.fb2" {
		t.Fatalf("plain Read() = %q, %q, %v", got, name, err)
	}

	archive := testZIP(t, map[string][]byte{
		"META-INF/info.txt": []byte("metadata"),
		"book.fb2":          plain,
	})
	got, name, err = Read(bytes.NewReader(archive), "book.fb2.zip", 4096)
	if err != nil || !bytes.Equal(got, plain) || name != "book.fb2" {
		t.Fatalf("ZIP Read() = %q, %q, %v", got, name, err)
	}
}

func TestReadZIPSelectionAndLimits(t *testing.T) {
	t.Parallel()
	archive := testZIP(t, map[string][]byte{
		"other.fb2": []byte("other"),
		"book.fb2":  []byte("selected"),
	})
	got, name, err := Read(bytes.NewReader(archive), "book.zip", 4096)
	if err != nil || string(got) != "selected" || name != "book.fb2" {
		t.Fatalf("matching candidate = %q, %q, %v", got, name, err)
	}

	ambiguous := testZIP(t, map[string][]byte{
		"one.fb2": []byte("one"),
		"two.fb2": []byte("two"),
	})
	if _, _, err := Read(bytes.NewReader(ambiguous), "archive.zip", 4096); !errors.Is(err, ErrAmbiguousFB2) {
		t.Fatalf("ambiguous error = %v", err)
	}
	if _, _, err := Read(bytes.NewReader([]byte("12345")), "book.fb2", 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size error = %v", err)
	}
}

func TestReadDetectsZIPBySignature(t *testing.T) {
	t.Parallel()
	archive := testZIP(t, map[string][]byte{"stdin.fb2": []byte("fb2")})
	got, name, err := Read(bytes.NewReader(archive), "", 4096)
	if err != nil || string(got) != "fb2" || name != "stdin.fb2" {
		t.Fatalf("signature Read() = %q, %q, %v", got, name, err)
	}
}

func testZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, data := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	return output.Bytes()
}
