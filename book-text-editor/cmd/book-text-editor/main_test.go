package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"book-text-editor/internal/book"
)

func TestRunKeepsLongSentenceIntactAndReportsWarning(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(
		[]string{"-max-words=2", "-"},
		strings.NewReader(cliTestDocument),
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	expected := "" +
		"Название: Книга\n" +
		"Авторы: Автор\n" +
		"Главы: 1\n" +
		"1. Глава — 1 сегм.\n" +
		"Всего сегментов: 1\n"
	if stdout.String() != expected {
		t.Fatalf("stdout = %q, want %q", stdout.String(), expected)
	}
	for _, expectedWarning := range []string{
		"Предупреждение",
		"4 слов",
		"лимит 2",
		"сохранена целиком",
		"Один два три четыре.",
	} {
		if !strings.Contains(stderr.String(), expectedWarning) {
			t.Errorf("stderr = %q, want %q", stderr.String(), expectedWarning)
		}
	}
}

func TestRunParsesFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "book.fb2")
	if err := os.WriteFile(path, []byte(cliTestDocument), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(
		[]string{"-max-words=2", path},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Всего сегментов: 1") {
		t.Fatalf("stdout = %q, want segment count", stdout.String())
	}
	if !strings.Contains(stderr.String(), "сохранена целиком") {
		t.Fatalf("stderr = %q, want long phrase warning", stderr.String())
	}
}

func TestRunDoesNotWarnForPhrasesWithinLimit(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	document := strings.Replace(
		cliTestDocument,
		"Один два три четыре.",
		"Один два. Три четыре.",
		1,
	)
	err := run(
		[]string{"-max-words=2", "-"},
		strings.NewReader(document),
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Всего сегментов: 2") {
		t.Fatalf("stdout = %q, want two segments", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no warnings", stderr.String())
	}
}

func TestRunRequiresOneInput(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	err := run(nil, strings.NewReader(""), io.Discard, &stderr)
	if err == nil {
		t.Fatal("run() returned nil error")
	}
	if !strings.Contains(stderr.String(), "Использование:") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func TestRunHandlesHelp(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	err := run([]string{"-h"}, strings.NewReader(""), io.Discard, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("run(-h) error = %v, want %v", err, flag.ErrHelp)
	}
	if !strings.Contains(stderr.String(), "Использование:") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func TestRunRejectsInvalidWordLimit(t *testing.T) {
	t.Parallel()

	err := run(
		[]string{"-max-words=0", "-"},
		strings.NewReader(""),
		io.Discard,
		io.Discard,
	)
	if err == nil {
		t.Fatal("run() returned nil error")
	}
	if !strings.Contains(err.Error(), "max words must be positive") {
		t.Fatalf("run() error = %v, want invalid word limit", err)
	}
}

func TestWriteSummaryPropagatesWriterError(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("write failed")
	err := writeSummary(errorWriter{err: expectedErr}, book.Book{Title: "Книга"})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("writeSummary() error = %v, want %v", err, expectedErr)
	}
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

const cliTestDocument = `<?xml version="1.0" encoding="utf-8"?>` +
	`<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">` +
	`<description><title-info>` +
	`<book-title>Книга</book-title>` +
	`<author><nickname>Автор</nickname></author>` +
	`</title-info></description>` +
	`<body><section><title><p>Глава</p></title>` +
	`<p>Один два три четыре.</p></section></body>` +
	`</FictionBook>`
