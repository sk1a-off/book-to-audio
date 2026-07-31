package book

import "testing"

func TestSegmentCount(t *testing.T) {
	t.Parallel()

	parsedBook := Book{
		Chapters: []Chapter{
			{Segments: []string{"one", "two"}},
			{Segments: []string{"three"}},
		},
	}

	if actual := parsedBook.SegmentCount(); actual != 3 {
		t.Fatalf("SegmentCount() = %d, want 3", actual)
	}
}
