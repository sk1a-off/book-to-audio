// Command book-text-editor prepares text from an FB2 document.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"book-text-editor/internal/book"
	"book-text-editor/internal/fb2"
	"book-text-editor/internal/segment"
)

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
			"Использование: book-text-editor [параметры] <book.fb2|->",
		)
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return fmt.Errorf("ожидался ровно один путь к FB2-файлу")
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
		parsedBook, err := parser.Parse(stdin)
		if err != nil {
			return book.Book{}, fmt.Errorf("разобрать FB2 из stdin: %w", err)
		}

		return parsedBook, nil
	}

	// #nosec G304 -- this local CLI intentionally opens the path supplied by its user.
	file, err := os.Open(path)
	if err != nil {
		return book.Book{}, fmt.Errorf("открыть FB2 %q: %w", path, err)
	}

	parsedBook, parseErr := parser.Parse(file)
	if parseErr != nil {
		parseErr = fmt.Errorf("разобрать FB2 %q: %w", path, parseErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		closeErr = fmt.Errorf("закрыть FB2 %q: %w", path, closeErr)
		parseErr = errors.Join(parseErr, closeErr)
	}
	if parseErr != nil {
		return book.Book{}, parseErr
	}

	return parsedBook, nil
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
