// Package book contains the application domain model.
package book

// Book is the normalized representation of an FB2 book.
type Book struct {
	Title    string
	Authors  []string
	Chapters []Chapter
}

// SegmentCount returns the total number of text segments in the book.
func (b Book) SegmentCount() int {
	count := 0
	for _, chapter := range b.Chapters {
		count += len(chapter.Segments)
	}

	return count
}

// Chapter is a flat, sequential unit prepared for further text processing.
type Chapter struct {
	Number   int
	Title    string
	Segments []string
}
