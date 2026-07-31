package fb2

import (
	"encoding/xml"
	"strings"
)

type metadataState struct {
	titleInfoDepth int
	bookTitle      *textCapture
	author         *authorCapture
	title          string
	authors        []string
}

type textCapture struct {
	element string
	depth   int
	text    strings.Builder
}

type authorCapture struct {
	depth int
	field *textCapture
	parts []string
}

func (s *metadataState) start(name, parent xml.Name, depth int) {
	switch {
	case isFB2Element(name, "title-info") &&
		isFB2Element(parent, "description") &&
		depth == 3 &&
		s.titleInfoDepth == 0:
		s.titleInfoDepth = depth

	case s.titleInfoDepth == 0:
		return

	case isFB2Element(name, "book-title") &&
		depth == s.titleInfoDepth+1 &&
		s.bookTitle == nil:
		s.bookTitle = &textCapture{element: name.Local, depth: depth}

	case isFB2Element(name, "author") &&
		depth == s.titleInfoDepth+1 &&
		s.author == nil:
		s.author = &authorCapture{depth: depth}

	case s.author != nil &&
		depth == s.author.depth+1 &&
		s.author.field == nil &&
		isAuthorNameField(name):
		s.author.field = &textCapture{element: name.Local, depth: depth}
	}
}

func (s *metadataState) writeText(data []byte) {
	if s.bookTitle != nil {
		s.bookTitle.text.Write(data)
	}
	if s.author != nil && s.author.field != nil {
		s.author.field.text.Write(data)
	}
}

func (s *metadataState) end(name xml.Name, depth int) {
	if s.author != nil &&
		s.author.field != nil &&
		s.author.field.element == name.Local &&
		name.Space == Namespace &&
		s.author.field.depth == depth {
		if value := normalizeSpace(s.author.field.text.String()); value != "" {
			s.author.parts = append(s.author.parts, value)
		}
		s.author.field = nil
	}

	if s.bookTitle != nil &&
		s.bookTitle.element == name.Local &&
		name.Space == Namespace &&
		s.bookTitle.depth == depth {
		s.title = normalizeSpace(s.bookTitle.text.String())
		s.bookTitle = nil
	}

	if s.author != nil && isFB2Element(name, "author") && s.author.depth == depth {
		if author := normalizeSpace(strings.Join(s.author.parts, " ")); author != "" {
			s.authors = append(s.authors, author)
		}
		s.author = nil
	}

	if isFB2Element(name, "title-info") && s.titleInfoDepth == depth {
		s.titleInfoDepth = 0
	}
}

func isAuthorNameField(name xml.Name) bool {
	if name.Space != Namespace {
		return false
	}

	switch name.Local {
	case "first-name", "middle-name", "last-name", "nickname":
		return true
	default:
		return false
	}
}
