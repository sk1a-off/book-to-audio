package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunValidatesRepositoryCorpus(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"-corpus", "../../../testdata/tts_quality/corpus.json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "language=ru-RU") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunRejectsUnexpectedArguments(t *testing.T) {
	t.Parallel()
	if err := run([]string{"unexpected"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("run() returned nil error")
	}
}
