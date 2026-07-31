// Package fb2 parses FictionBook 2.0 documents into the application model.
package fb2

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"book-text-editor/internal/book"
	"book-text-editor/internal/segment"

	"golang.org/x/net/html/charset"
)

// Namespace is the XML namespace used by FictionBook 2.0.
const Namespace = "http://www.gribuser.ru/xml/fictionbook/2.0"

var (
	// ErrInvalidInput indicates that the parser received no reader.
	ErrInvalidInput = errors.New("FB2 input is nil")
	// ErrNotFB2 indicates that the XML root is not a FictionBook 2.0 element.
	ErrNotFB2 = errors.New("input is not a FictionBook 2.0 document")
	// ErrNoBody indicates that the document has no main book body.
	ErrNoBody = errors.New("FB2 document has no main body")
	// ErrNoContent indicates that the main body has no readable text.
	ErrNoContent = errors.New("FB2 document has no readable text")
	// ErrTrailingData indicates content after the closing FictionBook element.
	ErrTrailingData = errors.New("FB2 document has trailing data")
)

// Parser converts a FictionBook 2.0 XML stream into a Book.
type Parser struct {
	segmenter segment.Segmenter
}

// NewParser creates a parser with the supplied text segmenter.
func NewParser(segmenter segment.Segmenter) Parser {
	return Parser{segmenter: segmenter}
}

// Parse reads one uncompressed FictionBook 2.0 XML document.
//
// Only the first body is treated as the main text. Subsequent bodies, commonly
// used for notes, are intentionally excluded from the chapter stream.
func (p Parser) Parse(reader io.Reader) (book.Book, error) {
	if reader == nil {
		return book.Book{}, ErrInvalidInput
	}

	document, err := decodeDocument(reader)
	if err != nil {
		return book.Book{}, err
	}
	if !document.rootSeen {
		return book.Book{}, ErrNotFB2
	}
	if !document.state.body.seen {
		return book.Book{}, ErrNoBody
	}

	result := book.Book{
		Title:    document.state.metadata.title,
		Authors:  append([]string(nil), document.state.metadata.authors...),
		Chapters: document.state.body.chapters(p.segmenter),
	}
	if result.Title == "" {
		result.Title = document.state.body.title
	}
	if len(result.Chapters) == 0 {
		return book.Book{}, ErrNoContent
	}

	return result, nil
}

type decodedDocument struct {
	state      documentState
	rootSeen   bool
	rootClosed bool
}

func decodeDocument(reader io.Reader) (decodedDocument, error) {
	decoder := xml.NewDecoder(reader)
	decoder.CharsetReader = charset.NewReaderLabel

	document := decodedDocument{}

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return decodedDocument{}, fmt.Errorf("decode FB2 XML: %w", err)
		}
		if err := document.consume(token); err != nil {
			return decodedDocument{}, err
		}
	}

	return document, nil
}

func (d *decodedDocument) consume(token xml.Token) error {
	switch element := token.(type) {
	case xml.StartElement:
		return d.consumeStart(element)
	case xml.CharData:
		return d.consumeText(element)
	case xml.EndElement:
		d.consumeEnd(element)
	case xml.Directive:
		if d.rootClosed {
			return ErrTrailingData
		}
	case xml.ProcInst:
		if strings.EqualFold(element.Target, "xml") && d.rootSeen {
			return ErrTrailingData
		}
	}

	return nil
}

func (d *decodedDocument) consumeStart(element xml.StartElement) error {
	if d.rootClosed {
		return ErrTrailingData
	}
	if !d.rootSeen {
		if !isFB2Element(element.Name, "FictionBook") {
			return ErrNotFB2
		}
		d.rootSeen = true
	}

	d.state.start(element)

	return nil
}

func (d *decodedDocument) consumeText(data xml.CharData) error {
	if !d.rootSeen {
		if len(bytes.TrimSpace(data)) > 0 {
			return ErrNotFB2
		}
		return nil
	}
	if d.rootClosed {
		if len(bytes.TrimSpace(data)) > 0 {
			return ErrTrailingData
		}
		return nil
	}

	d.state.writeText(data)

	return nil
}

func (d *decodedDocument) consumeEnd(element xml.EndElement) {
	closingRoot := len(d.state.elements) == 1 &&
		isFB2Element(element.Name, "FictionBook")
	d.state.end(element)
	if closingRoot {
		d.rootClosed = true
	}
}

func isFB2Element(name xml.Name, local string) bool {
	return name.Local == local && name.Space == Namespace
}

type documentState struct {
	elements []xml.Name
	metadata metadataState
	body     bodyState
}

func (s *documentState) start(element xml.StartElement) {
	depth := len(s.elements) + 1
	parent := xml.Name{}
	if len(s.elements) > 0 {
		parent = s.elements[len(s.elements)-1]
	}

	s.metadata.start(element.Name, parent, depth)
	s.body.start(element, parent, depth)
	s.elements = append(s.elements, element.Name)
}

func (s *documentState) writeText(data xml.CharData) {
	s.metadata.writeText(data)
	s.body.writeText(data)
}

func (s *documentState) end(element xml.EndElement) {
	depth := len(s.elements)
	s.metadata.end(element.Name, depth)
	s.body.end(element.Name, depth)

	if depth > 0 {
		s.elements = s.elements[:depth-1]
	}
}
