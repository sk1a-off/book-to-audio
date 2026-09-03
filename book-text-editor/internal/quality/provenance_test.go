package quality

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const repositoryRoot = "../../.."

type hashedAsset struct {
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
}

func TestBaselineAssetsMatchRecordedProvenance(t *testing.T) {
	t.Parallel()
	var baseline struct {
		Source           hashedAsset `json:"source"`
		SpeakerReference struct {
			RelativePath           string `json:"relative_path"`
			SHA256                 string `json:"sha256"`
			TranscriptRelativePath string `json:"transcript_relative_path"`
			TranscriptSHA256       string `json:"transcript_sha256"`
		} `json:"speaker_reference"`
	}
	decodeJSONFile(t, filepath.Join(
		repositoryRoot,
		"testdata/tts_quality/baselines/current_omnivoice_2026-08-18.json",
	), &baseline)

	assertAssetHash(t, baseline.Source.RelativePath, baseline.Source.SHA256, baseline.Source.SizeBytes)
	assertAssetHash(t, baseline.SpeakerReference.RelativePath, baseline.SpeakerReference.SHA256, 0)
	assertAssetHash(t, baseline.SpeakerReference.TranscriptRelativePath, baseline.SpeakerReference.TranscriptSHA256, 0)
}

func TestQualityJSONArtifactsAreWellFormed(t *testing.T) {
	t.Parallel()
	paths := []string{
		"testdata/tts_quality/manifests/schema.json",
		"testdata/tts_quality/ratings/schema.json",
		"testdata/tts_quality/ratings/session-schema.json",
		"testdata/tts_quality/references/manifest.json",
	}
	for _, path := range paths {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			var value any
			decodeJSONFile(t, filepath.Join(repositoryRoot, path), &value)
		})
	}
}

func decodeJSONFile(t *testing.T, path string, destination any) {
	t.Helper()
	file, err := os.Open(path) // #nosec G304 -- tests open fixed repository fixtures.
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(destination); err != nil {
		_ = file.Close()
		t.Fatalf("decode %s: %v", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		_ = file.Close()
		t.Fatalf("%s contains trailing JSON: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func assertAssetHash(t *testing.T, relativePath, wantHash string, wantSize int64) {
	t.Helper()
	path := filepath.Join(repositoryRoot, filepath.FromSlash(relativePath))
	file, err := os.Open(path) // #nosec G304 -- paths come from version-controlled test metadata.
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		t.Fatal(fmt.Errorf("hash %s: %w", path, copyErr))
	}
	if closeErr != nil {
		t.Fatal(fmt.Errorf("close %s: %w", path, closeErr))
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != wantHash {
		t.Errorf("sha256(%s) = %s, want %s", relativePath, got, wantHash)
	}
	if wantSize > 0 && size != wantSize {
		t.Errorf("size(%s) = %d, want %d", relativePath, size, wantSize)
	}
}
