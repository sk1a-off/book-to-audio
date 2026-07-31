// Command book-text-editor prepares text from a plain FB2 or FB2.ZIP document.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"book-text-editor/internal/book"
	"book-text-editor/internal/bookinput"
	"book-text-editor/internal/fb2"
	"book-text-editor/internal/segment"
)

const maxCLIInputSize int64 = 50 << 20

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "Ошибка: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("book-text-editor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	maxWords := flags.Int(
		"max-words",
		segment.DefaultMaxWords,
		"максимальное количество слов в одном сегменте",
	)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(
			stderr,
			"Использование: book-text-editor [параметры] <book.fb2|book.fb2.zip|->",
		)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return fmt.Errorf("ожидался ровно один путь к FB2 или FB2.ZIP")
	}

	textSegmenter, err := segment.New(*maxWords)
	if err != nil {
		return fmt.Errorf("некорректный параметр -max-words: %w", err)
	}
	textSegmenter = textSegmenter.WithWarningHandler(func(warning segment.Warning) {
		_, _ = fmt.Fprintf(
			stderr,
			"Предупреждение: фраза из %d слов превышает лимит %d и сохранена целиком: %q\n",
			warning.Words,
			warning.Limit,
			warning.Phrase,
		)
	})

	parser := fb2.NewParser(textSegmenter)
	parsedBook, err := parseInput(flags.Arg(0), stdin, parser)
	if err != nil {
		return err
	}
	if err := writeSummary(stdout, parsedBook); err != nil {
		return fmt.Errorf("вывести результат: %w", err)
	}
	return nil
}

func parseInput(path string, stdin io.Reader, parser fb2.Parser) (book.Book, error) {
	if path == "-" {
		data, name, err := bookinput.Read(stdin, "", maxCLIInputSize)
		if err != nil {
			return book.Book{}, fmt.Errorf("прочитать FB2/ZIP из stdin: %w", err)
		}
		parsedBook, err := parser.Parse(bytes.NewReader(data))
		if err != nil {
			return book.Book{}, fmt.Errorf("разобрать %s из stdin: %w", inputLabel(name), err)
		}
		return parsedBook, nil
	}

	// #nosec G304 -- this local CLI intentionally opens the path supplied by its user.
	file, err := os.Open(path)
	if err != nil {
		return book.Book{}, fmt.Errorf("открыть книгу %q: %w", path, err)
	}
	data, selectedName, readErr := bookinput.Read(file, path, maxCLIInputSize)
	closeErr := file.Close()
	if readErr != nil {
		readErr = fmt.Errorf("прочитать книгу %q: %w", path, readErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("закрыть книгу %q: %w", path, closeErr)
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return book.Book{}, err
	}
	parsedBook, err := parser.Parse(bytes.NewReader(data))
	if err != nil {
		return book.Book{}, fmt.Errorf(
			"разобрать %s %q: %w",
			inputLabel(selectedName),
			path,
			err,
		)
	}
	return parsedBook, nil
}

func inputLabel(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".fb2") {
		return "FB2"
	}
	return "книгу"
}

func writeSummary(writer io.Writer, parsedBook book.Book) error {
	output := bufio.NewWriter(writer)
	var writeErr error
	write := func(format string, args ...any) {
		if writeErr != nil {
			return
		}
		_, writeErr = fmt.Fprintf(output, format, args...)
	}

	title := parsedBook.Title
	if title == "" {
		title = "(без названия)"
	}
	authors := "(не указаны)"
	if len(parsedBook.Authors) > 0 {
		authors = strings.Join(parsedBook.Authors, ", ")
	}
	write("Название: %s\n", title)
	write("Авторы: %s\n", authors)
	write("Главы: %d\n", len(parsedBook.Chapters))
	for _, chapter := range parsedBook.Chapters {
		chapterTitle := chapter.Title
		if chapterTitle == "" {
			chapterTitle = "(без названия)"
		}
		write("%d. %s — %d сегм.\n", chapter.Number, chapterTitle, len(chapter.Segments))
	}
	write("Всего сегментов: %d\n", parsedBook.SegmentCount())
	if flushErr := output.Flush(); writeErr == nil {
		writeErr = flushErr
	}
	return writeErr
}
