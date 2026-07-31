// Package bookinput reads either a plain FB2 document or an FB2 stored in ZIP.
package bookinput

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

var (
	ErrTooLarge     = errors.New("book input exceeds the size limit")
	ErrNoFB2        = errors.New("ZIP archive does not contain an FB2 document")
	ErrAmbiguousFB2 = errors.New("ZIP archive contains multiple ambiguous FB2 documents")
	ErrInvalidZIP   = errors.New("book ZIP archive is invalid")
)

// Read returns uncompressed FB2 bytes and the selected source name. ZIP is
// detected by signature as well as filename, so stdin and incorrectly named
// uploads still work. Neither archive entries nor temporary files are written
// to disk.
func Read(reader io.Reader, filename string, limit int64) ([]byte, string, error) {
	if reader == nil {
		return nil, "", errors.New("book reader is nil")
	}
	if limit <= 0 {
		return nil, "", errors.New("book size limit must be positive")
	}
	payload, err := readLimited(reader, limit)
	if err != nil {
		return nil, "", err
	}
	if !looksLikeZIP(payload, filename) {
		return payload, filename, nil
	}
	return readZIP(payload, filename, limit)
}

func readZIP(payload []byte, archiveName string, limit int64) ([]byte, string, error) {
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalidZIP, err)
	}
	candidates := make([]*zip.File, 0)
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || !strings.EqualFold(path.Ext(file.Name), ".fb2") {
			continue
		}
		if file.UncompressedSize64 > uint64(limit) {
			return nil, "", ErrTooLarge
		}
		candidates = append(candidates, file)
	}
	if len(candidates) == 0 {
		return nil, "", ErrNoFB2
	}
	selected, err := selectCandidate(candidates, archiveName)
	if err != nil {
		return nil, "", err
	}
	entry, err := selected.Open()
	if err != nil {
		return nil, "", fmt.Errorf("%w: open %q: %v", ErrInvalidZIP, selected.Name, err)
	}
	defer entry.Close()
	data, err := readLimited(entry, limit)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("%w: read %q: %v", ErrInvalidZIP, selected.Name, err)
	}
	return data, selected.Name, nil
}

func selectCandidate(candidates []*zip.File, archiveName string) (*zip.File, error) {
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	base := strings.TrimSuffix(path.Base(strings.ReplaceAll(archiveName, "\\", "/")), ".zip")
	base = strings.TrimSuffix(base, ".ZIP")
	base = strings.TrimSuffix(base, ".fb2")
	base = strings.TrimSuffix(base, ".FB2")
	matches := make([]*zip.File, 0, 1)
	for _, candidate := range candidates {
		candidateBase := strings.TrimSuffix(path.Base(candidate.Name), path.Ext(candidate.Name))
		if base != "" && strings.EqualFold(candidateBase, base) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	// Deterministic ordering makes the error stable without silently choosing a
	// possibly wrong book.
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].Name < candidates[right].Name
	})
	return nil, ErrAmbiguousFB2
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

func looksLikeZIP(payload []byte, filename string) bool {
	if len(payload) >= 4 && payload[0] == 'P' && payload[1] == 'K' &&
		(payload[2] == 3 || payload[2] == 5 || payload[2] == 7) &&
		(payload[3] == 4 || payload[3] == 6 || payload[3] == 8) {
		return true
	}
	return strings.EqualFold(path.Ext(filename), ".zip")
}
