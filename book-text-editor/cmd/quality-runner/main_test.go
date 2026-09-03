package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunGeneratesControlledSampleAndManifest(t *testing.T) {
	pcm := make([]byte, 48_000)
	for offset := 0; offset < len(pcm); offset += 2 {
		binary.LittleEndian.PutUint16(pcm[offset:offset+2], uint16(4_000))
	}
	digest := sha256.Sum256(pcm)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/generate" || request.Method != http.MethodPost {
			http.NotFound(writer, request)
			return
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm() error = %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var metadata struct {
			RequestID string `json:"request_id"`
			Text      string `json:"text"`
			Seed      uint32 `json:"seed"`
		}
		if err := json.Unmarshal([]byte(request.FormValue("metadata")), &metadata); err != nil {
			t.Errorf("decode metadata: %v", err)
		}
		if metadata.RequestID == "" || metadata.Text == "" || metadata.Seed != 42 {
			t.Errorf("unexpected metadata: %+v", metadata)
		}
		writer.Header().Set("X-Request-ID", metadata.RequestID)
		writer.Header().Set("X-Audio-Sample-Rate", "24000")
		writer.Header().Set("X-Audio-Channels", "1")
		writer.Header().Set("X-Audio-Bits-Per-Sample", "16")
		writer.Header().Set("X-Audio-Duration-Ms", "1000")
		writer.Header().Set("X-Generation-Duration-Ms", "250")
		writer.Header().Set("X-Seed-Used", "42")
		writer.Header().Set("X-Voice-Cache-Hit", "false")
		writer.Header().Set("X-Model-ID", "fake/omnivoice")
		writer.Header().Set("X-Model-Version", "fake-revision")
		writer.Header().Set("X-Audio-SHA256", hex.EncodeToString(digest[:]))
		_, _ = writer.Write(pcm)
	}))
	defer server.Close()

	temporary := t.TempDir()
	referencePath := filepath.Join(temporary, "reference.flac")
	transcriptPath := filepath.Join(temporary, "reference.txt")
	if err := os.WriteFile(referencePath, []byte("fake-reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte("Текст референса."), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	var stderr strings.Builder
	err := run(context.Background(), []string{
		"-experiment", "fake-smoke",
		"-endpoint", server.URL,
		"-corpus", "../../../testdata/tts_quality/corpus.json",
		"-output", filepath.Join(temporary, "outputs"),
		"-reference", referencePath,
		"-reference-text", transcriptPath,
		"-cases", "prose-long-period",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "samples=1") {
		t.Errorf("stdout = %q", stdout.String())
	}

	experimentDir := filepath.Join(temporary, "outputs", "fake-smoke")
	for _, name := range []string{
		"experiment.json",
		"prose-long-period.json",
		"prose-long-period.wav",
	} {
		if info, err := os.Stat(filepath.Join(experimentDir, name)); err != nil || info.Size() == 0 {
			t.Errorf("artifact %s: info=%v err=%v", name, info, err)
		}
	}
	manifestFile, err := os.Open(filepath.Join(experimentDir, "prose-long-period.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Model    string `json:"model"`
		Artifact struct {
			DurationSeconds float64 `json:"duration_seconds"`
			SHA256          string  `json:"sha256"`
		} `json:"artifact"`
	}
	if err := json.NewDecoder(manifestFile).Decode(&manifest); err != nil {
		_ = manifestFile.Close()
		t.Fatal(err)
	}
	if err := manifestFile.Close(); err != nil {
		t.Fatal(err)
	}
	if manifest.Model != "fake/omnivoice" || manifest.Artifact.DurationSeconds != 1 ||
		len(manifest.Artifact.SHA256) != 64 {
		t.Errorf("unexpected manifest: %+v", manifest)
	}
}

func TestRunRequiresExperimentID(t *testing.T) {
	t.Parallel()
	err := run(context.Background(), nil, io.Discard, io.Discard)
	if err == nil || err.Error() != "-experiment is required" {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunRejectsOutOfRangeAdaptivePause(t *testing.T) {
	t.Parallel()
	err := run(context.Background(), []string{
		"-experiment", "bad-pause",
		"-inter-chunk-min-pause-ms", "5001",
	}, io.Discard, io.Discard)
	if err == nil || err.Error() != "-inter-chunk-min-pause-ms must be between 0 and 5000" {
		t.Fatalf("run() error = %v", err)
	}
}

func TestParsePauseSequence(t *testing.T) {
	t.Parallel()
	got, err := parsePauseSequence("800, 500")
	if err != nil || len(got) != 2 || got[0] != 800 || got[1] != 500 {
		t.Fatalf("parsePauseSequence() = %v, %v", got, err)
	}
	for _, value := range []string{"500,", "-1", "5001", "word"} {
		if _, err := parsePauseSequence(value); err == nil {
			t.Errorf("parsePauseSequence(%q) error = nil", value)
		}
	}
}

func TestReferenceContentType(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"voice.FLAC": "audio/flac",
		"voice.wav":  "audio/wav",
	} {
		got, err := referenceContentType(path)
		if err != nil || got != want {
			t.Errorf("referenceContentType(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := referenceContentType("voice.mp3"); err == nil {
		t.Fatal("referenceContentType(mp3) error = nil")
	}
}

func ExampleRun() {
	_, _ = fmt.Fprintln(io.Discard, "quality-runner requires a live worker")
	// Output:
}
