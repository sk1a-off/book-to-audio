package fb2

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"book-text-editor/internal/segment"

	"golang.org/x/text/encoding/charmap"
)

func TestParseComplexDocument(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/book.fb2")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	textSegmenter, err := segment.New(100)
	if err != nil {
		t.Fatalf("segment.New() error = %v", err)
	}

	parsedBook, err := NewParser(textSegmenter).Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if parsedBook.Title != "Тестовая книга" {
		t.Fatalf("Title = %q, want %q", parsedBook.Title, "Тестовая книга")
	}
	if expected := []string{"Иван И. Иванов"}; !slices.Equal(parsedBook.Authors, expected) {
		t.Fatalf("Authors = %q, want %q", parsedBook.Authors, expected)
	}
	if len(parsedBook.Chapters) != 2 {
		t.Fatalf("len(Chapters) = %d, want 2", len(parsedBook.Chapters))
	}

	first := parsedBook.Chapters[0]
	if first.Number != 1 {
		t.Errorf("first chapter Number = %d, want 1", first.Number)
	}
	if first.Title != "Часть I — Глава 1" {
		t.Errorf("first chapter Title = %q, want %q", first.Title, "Часть I — Глава 1")
	}
	expectedFirstSegments := []string{
		"Эпиграф. Автор эпиграфа Первый абзац. Название стиха Строка. Поэт Ячейка",
	}
	if !slices.Equal(first.Segments, expectedFirstSegments) {
		t.Errorf("first chapter Segments = %q, want %q", first.Segments, expectedFirstSegments)
	}

	second := parsedBook.Chapters[1]
	if second.Number != 2 {
		t.Errorf("second chapter Number = %d, want 2", second.Number)
	}
	if second.Title != "Часть I" {
		t.Errorf("second chapter Title = %q, want %q", second.Title, "Часть I")
	}
	if expected := []string{"Без заголовка."}; !slices.Equal(second.Segments, expected) {
		t.Errorf("second chapter Segments = %q, want %q", second.Segments, expected)
	}

	for _, chapter := range parsedBook.Chapters {
		for _, text := range chapter.Segments {
			if strings.Contains(text, "не входит") {
				t.Fatal("text from the notes body leaked into the main chapters")
			}
		}
	}
}

func TestParseUsesBodyTitleAsFallback(t *testing.T) {
	t.Parallel()

	document := validFB2(`
		<body>
			<title><p>Название из body</p></title>
			<section><p>Текст.</p></section>
		</body>`)

	parsedBook, err := NewParser(segment.Segmenter{}).Parse(strings.NewReader(document))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsedBook.Title != "Название из body" {
		t.Fatalf("Title = %q, want %q", parsedBook.Title, "Название из body")
	}
}

func TestParseKeepsSectionBoundariesForEqualTitles(t *testing.T) {
	t.Parallel()

	document := validFB2(`
		<body>
			<section>
				<title><p>Часть</p></title>
				<section><title><p>Глава</p></title><p>Первая.</p></section>
				<section><title><p>Глава</p></title><p>Вторая.</p></section>
			</section>
		</body>`)

	parsedBook, err := NewParser(segment.Segmenter{}).Parse(strings.NewReader(document))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsedBook.Chapters) != 2 {
		t.Fatalf("len(Chapters) = %d, want 2", len(parsedBook.Chapters))
	}

	for index, expectedText := range []string{"Первая.", "Вторая."} {
		chapter := parsedBook.Chapters[index]
		if chapter.Title != "Часть — Глава" {
			t.Errorf("chapter %d Title = %q, want %q", index, chapter.Title, "Часть — Глава")
		}
		if expected := []string{expectedText}; !slices.Equal(chapter.Segments, expected) {
			t.Errorf("chapter %d Segments = %q, want %q", index, chapter.Segments, expected)
		}
	}
}

func TestParsePreservesBoundariesInsideTextContainers(t *testing.T) {
	t.Parallel()

	document := validFB2(`
		<body>
			<title><p>Название</p><p>книги</p></title>
			<section>
				<title><p>Глава</p><p>первая</p></title>
				<poem><title><p>Название</p><p>стиха</p></title></poem>
				<p>До<image alt="иллюстрация"/>после.</p>
				<p>Без<image/>склейки.</p>
			</section>
		</body>`)

	parsedBook, err := NewParser(segment.Segmenter{}).Parse(strings.NewReader(document))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsedBook.Title != "Название книги" {
		t.Errorf("Title = %q, want %q", parsedBook.Title, "Название книги")
	}
	if len(parsedBook.Chapters) != 1 {
		t.Fatalf("len(Chapters) = %d, want 1", len(parsedBook.Chapters))
	}

	chapter := parsedBook.Chapters[0]
	if chapter.Title != "Глава первая" {
		t.Errorf("chapter Title = %q, want %q", chapter.Title, "Глава первая")
	}
	expected := []string{
		"Название стиха До иллюстрация после. Без склейки.",
	}
	if !slices.Equal(chapter.Segments, expected) {
		t.Errorf("chapter Segments = %q, want %q", chapter.Segments, expected)
	}
}

func TestParseRemovesSceneBreakAndForcesSegmentBoundary(t *testing.T) {
	t.Parallel()

	document := validFB2(`
		<body>
			<section>
				<p>До разделителя.</p>
				<subtitle>* * *</subtitle>
				<p>После разделителя.</p>
			</section>
		</body>`)

	textSegmenter, err := segment.New(100)
	if err != nil {
		t.Fatalf("segment.New() error = %v", err)
	}
	parsedBook, err := NewParser(textSegmenter).Parse(strings.NewReader(document))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	expected := []string{"До разделителя.", "После разделителя."}
	if actual := parsedBook.Chapters[0].Segments; !slices.Equal(actual, expected) {
		t.Fatalf("Segments = %q, want %q", actual, expected)
	}
}

func TestParseUsesOnlyOfficialNamespace(t *testing.T) {
	t.Parallel()

	document := `<?xml version="1.0"?>` +
		`<FictionBook xmlns="` + Namespace + `" xmlns:x="urn:not-fb2">` +
		`<x:description><x:title-info><x:book-title>Injected</x:book-title>` +
		`</x:title-info></x:description>` +
		`<x:body><x:section><x:p>Injected body.</x:p></x:section></x:body>` +
		`<body>` +
		`<x:section><x:p>Injected section.</x:p></x:section>` +
		`<section><p>Настоящий текст.</p></section>` +
		`</body></FictionBook>`

	parsedBook, err := NewParser(segment.Segmenter{}).Parse(strings.NewReader(document))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsedBook.Title != "" {
		t.Errorf("Title = %q, want empty title", parsedBook.Title)
	}
	if len(parsedBook.Chapters) != 1 {
		t.Fatalf("len(Chapters) = %d, want 1", len(parsedBook.Chapters))
	}
	expected := []string{"Настоящий текст."}
	if actual := parsedBook.Chapters[0].Segments; !slices.Equal(actual, expected) {
		t.Fatalf("Segments = %q, want %q", actual, expected)
	}
}

func TestParseAllowsTrailingXMLMisc(t *testing.T) {
	t.Parallel()

	document := validFB2(`<body><section><p>Текст.</p></section></body>`) +
		" \n<!-- trailing comment --><?trailing instruction?>"

	if _, err := NewParser(segment.Segmenter{}).Parse(strings.NewReader(document)); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParseRejectsInvalidDocuments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		document string
		target   error
	}{
		{
			name:   "empty input",
			target: ErrNotFB2,
		},
		{
			name:     "HTML",
			document: `<html><body><p>text</p></body></html>`,
			target:   ErrNotFB2,
		},
		{
			name: "text before root",
			document: `garbage<FictionBook xmlns="` + Namespace + `">` +
				`<body><section><p>text</p></section></body></FictionBook>`,
			target: ErrNotFB2,
		},
		{
			name: "wrong namespace",
			document: `<FictionBook xmlns="https://example.com/not-fb2">` +
				`<body><section><p>text</p></section></body></FictionBook>`,
			target: ErrNotFB2,
		},
		{
			name:     "missing body",
			document: validFB2(""),
			target:   ErrNoBody,
		},
		{
			name: "foreign body",
			document: `<?xml version="1.0"?>` +
				`<FictionBook xmlns="` + Namespace + `" xmlns:x="urn:not-fb2">` +
				`<x:body><x:section><x:p>text</x:p></x:section></x:body>` +
				`</FictionBook>`,
			target: ErrNoBody,
		},
		{
			name: "nested body",
			document: validFB2(
				`<extension><FictionBook><body>` +
					`<section><p>text</p></section>` +
					`</body></FictionBook></extension>`,
			),
			target: ErrNoBody,
		},
		{
			name: "missing readable content",
			document: validFB2(
				`<body><title><p>Только заголовок</p></title></body>`,
			),
			target: ErrNoContent,
		},
		{
			name: "second root element",
			document: validFB2(
				`<body><section><p>text</p></section></body>`,
			) + `<another/>`,
			target: ErrTrailingData,
		},
		{
			name: "text after root",
			document: validFB2(
				`<body><section><p>text</p></section></body>`,
			) + `trailing`,
			target: ErrTrailingData,
		},
		{
			name: "directive after root",
			document: validFB2(
				`<body><section><p>text</p></section></body>`,
			) + `<!unexpected>`,
			target: ErrTrailingData,
		},
		{
			name: "second XML declaration",
			document: validFB2(
				`<body><section><p>text</p></section></body>`,
			) + `<?xml version="1.0"?>`,
			target: ErrTrailingData,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewParser(segment.Segmenter{}).Parse(strings.NewReader(test.document))
			if !errors.Is(err, test.target) {
				t.Fatalf("Parse() error = %v, want errors.Is(_, %v)", err, test.target)
			}
		})
	}
}

func TestParseReturnsMalformedXMLError(t *testing.T) {
	t.Parallel()

	document := `<?xml version="1.0"?>` +
		`<FictionBook xmlns="` + Namespace + `"><body><section>`

	_, err := NewParser(segment.Segmenter{}).Parse(strings.NewReader(document))
	if err == nil {
		t.Fatal("Parse() returned nil error")
	}
	if errors.Is(err, ErrNoBody) || errors.Is(err, ErrNoContent) {
		t.Fatalf("Parse() error = %v, want XML decoder error", err)
	}
}

func TestParseWindows1251(t *testing.T) {
	t.Parallel()

	document := `<?xml version="1.0" encoding="windows-1251"?>` +
		`<FictionBook xmlns="` + Namespace + `">` +
		`<description><title-info>` +
		`<book-title>Книга</book-title>` +
		`<author><nickname>Автор</nickname></author>` +
		`</title-info></description>` +
		`<body><section><title><p>Глава</p></title><p>Текст.</p></section></body>` +
		`</FictionBook>`

	encoded, err := charmap.Windows1251.NewEncoder().Bytes([]byte(document))
	if err != nil {
		t.Fatalf("encode windows-1251: %v", err)
	}

	parsedBook, err := NewParser(segment.Segmenter{}).Parse(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsedBook.Title != "Книга" {
		t.Errorf("Title = %q, want %q", parsedBook.Title, "Книга")
	}
	if expected := []string{"Автор"}; !slices.Equal(parsedBook.Authors, expected) {
		t.Errorf("Authors = %q, want %q", parsedBook.Authors, expected)
	}
}

func TestParseRejectsNilReader(t *testing.T) {
	t.Parallel()

	_, err := NewParser(segment.Segmenter{}).Parse(nil)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Parse(nil) error = %v, want %v", err, ErrInvalidInput)
	}
}

func FuzzParseDoesNotPanic(f *testing.F) {
	f.Add([]byte(validFB2(`<body><section><p>Текст.</p></section></body>`)))
	f.Add([]byte(`<html><body>`))
	f.Add([]byte{})

	parser := NewParser(segment.Segmenter{})
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parser.Parse(bytes.NewReader(data))
	})
}

func validFB2(content string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<FictionBook xmlns="` + Namespace + `">` +
		content +
		`</FictionBook>`
}
