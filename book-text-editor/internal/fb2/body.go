package fb2

import (
	"encoding/xml"
	"strings"

	"book-text-editor/internal/book"
	"book-text-editor/internal/segment"
)

type bodyState struct {
	seen        bool
	activeDepth int
	title       string
	items       []contentItem
	sections    []sectionFrame
	titleText   *titleCapture
	blockText   *textCapture
}

type contentItem struct {
	text    string
	section *sectionNode
}

type sectionNode struct {
	title string
	items []contentItem
}

type sectionFrame struct {
	depth int
	node  *sectionNode
}

type titleCapture struct {
	depth   int
	section *sectionNode
	text    strings.Builder
}

type chapterDraft struct {
	title  string
	blocks []string
	source *sectionNode
}

func (s *bodyState) start(element xml.StartElement, parent xml.Name, depth int) {
	name := element.Name

	if isFB2Element(name, "body") &&
		isFB2Element(parent, "FictionBook") &&
		depth == 2 {
		if !s.seen {
			s.seen = true
			s.activeDepth = depth
		}
		return
	}
	if s.activeDepth == 0 || depth <= s.activeDepth {
		return
	}

	if isFB2Element(name, "section") && s.isDirectSection(parent, depth) {
		node := &sectionNode{}
		item := contentItem{section: node}

		if current := s.currentSection(); current != nil {
			current.items = append(current.items, item)
		} else {
			s.items = append(s.items, item)
		}

		s.sections = append(s.sections, sectionFrame{depth: depth, node: node})
		return
	}

	if isFB2Element(name, "title") && s.titleText == nil {
		switch {
		case isFB2Element(parent, "body") && depth == s.activeDepth+1:
			s.titleText = &titleCapture{depth: depth}
			return

		case isFB2Element(parent, "section") && s.isCurrentSectionParent(depth):
			s.titleText = &titleCapture{
				depth:   depth,
				section: s.currentSection(),
			}
			return
		}
	}

	if isFB2Element(name, "image") {
		s.appendImageText(element)
		return
	}

	if s.titleText != nil || s.blockText != nil {
		return
	}
	if isVisibleTextBlock(name) {
		s.blockText = &textCapture{element: name.Local, depth: depth}
	}
}

func (s *bodyState) writeText(data []byte) {
	switch {
	case s.titleText != nil:
		s.titleText.text.Write(data)
	case s.blockText != nil:
		s.blockText.text.Write(data)
	}
}

func (s *bodyState) end(name xml.Name, depth int) {
	if s.titleText != nil &&
		depth > s.titleText.depth &&
		isVisibleTextBlock(name) {
		s.titleText.text.WriteByte(' ')
	}
	if s.blockText != nil &&
		depth > s.blockText.depth &&
		isVisibleTextBlock(name) {
		s.blockText.text.WriteByte(' ')
	}

	if s.blockText != nil &&
		s.blockText.element == name.Local &&
		name.Space == Namespace &&
		s.blockText.depth == depth {
		s.appendText(normalizeSpace(s.blockText.text.String()))
		s.blockText = nil
	}

	if s.titleText != nil &&
		isFB2Element(name, "title") &&
		s.titleText.depth == depth {
		title := normalizeSpace(s.titleText.text.String())
		if s.titleText.section != nil {
			s.titleText.section.title = title
		} else {
			s.title = title
		}
		s.titleText = nil
	}

	if isFB2Element(name, "section") &&
		len(s.sections) > 0 &&
		s.sections[len(s.sections)-1].depth == depth {
		s.sections = s.sections[:len(s.sections)-1]
	}

	if isFB2Element(name, "body") && s.activeDepth == depth {
		s.activeDepth = 0
		s.sections = nil
		s.titleText = nil
		s.blockText = nil
	}
}

func (s *bodyState) isDirectSection(parent xml.Name, depth int) bool {
	if isFB2Element(parent, "body") {
		return depth == s.activeDepth+1
	}

	return isFB2Element(parent, "section") && s.isCurrentSectionParent(depth)
}

func (s *bodyState) isCurrentSectionParent(depth int) bool {
	return len(s.sections) > 0 && s.sections[len(s.sections)-1].depth == depth-1
}

func (s *bodyState) currentSection() *sectionNode {
	if len(s.sections) == 0 {
		return nil
	}

	return s.sections[len(s.sections)-1].node
}

func (s *bodyState) appendText(text string) {
	if text == "" {
		return
	}

	item := contentItem{text: text}
	if current := s.currentSection(); current != nil {
		current.items = append(current.items, item)
		return
	}

	s.items = append(s.items, item)
}

func (s *bodyState) appendImageText(element xml.StartElement) {
	alt := ""
	for _, attribute := range element.Attr {
		if attribute.Name.Local == "alt" {
			alt = normalizeSpace(attribute.Value)
			break
		}
	}

	switch {
	case s.titleText != nil:
		appendInlineText(&s.titleText.text, alt)
	case s.blockText != nil:
		appendInlineText(&s.blockText.text, alt)
	case alt != "":
		s.appendText(alt)
	}
}

func appendInlineText(builder *strings.Builder, text string) {
	builder.WriteByte(' ')
	builder.WriteString(text)
	builder.WriteByte(' ')
}

func (s *bodyState) chapters(segmenter segment.Segmenter) []book.Chapter {
	drafts := flattenBody(s.items, s.title)
	chapters := make([]book.Chapter, 0, len(drafts))

	for _, draft := range drafts {
		segments := segmenter.Split(strings.Join(draft.blocks, "\n"))
		if len(segments) == 0 {
			continue
		}

		chapters = append(chapters, book.Chapter{
			Number:   len(chapters) + 1,
			Title:    normalizeSpace(draft.title),
			Segments: segments,
		})
	}

	return chapters
}

func flattenBody(items []contentItem, bodyTitle string) []chapterDraft {
	var (
		drafts  []chapterDraft
		pending []string
	)

	for _, item := range items {
		if item.section == nil {
			pending = append(pending, item.text)
			continue
		}

		sectionDrafts := flattenSection(item.section, nil)
		if len(sectionDrafts) == 0 {
			continue
		}
		if len(pending) > 0 {
			sectionDrafts[0].blocks = append(
				append([]string(nil), pending...),
				sectionDrafts[0].blocks...,
			)
			pending = nil
		}

		drafts = append(drafts, sectionDrafts...)
	}

	if len(pending) > 0 {
		if len(drafts) == 0 {
			drafts = append(drafts, chapterDraft{
				title:  bodyTitle,
				blocks: pending,
			})
		} else {
			last := len(drafts) - 1
			drafts[last].blocks = append(drafts[last].blocks, pending...)
		}
	}

	return drafts
}

func flattenSection(node *sectionNode, ancestors []string) []chapterDraft {
	path := append([]string(nil), ancestors...)
	if node.title != "" {
		path = append(path, node.title)
	}
	title := strings.Join(path, " — ")

	var (
		drafts  []chapterDraft
		pending []string
	)

	flush := func() {
		if len(pending) == 0 {
			return
		}

		appendDraft(&drafts, chapterDraft{
			title:  title,
			blocks: pending,
			source: node,
		})
		pending = nil
	}

	for _, item := range node.items {
		if item.section == nil {
			pending = append(pending, item.text)
			continue
		}

		flush()
		for _, childDraft := range flattenSection(item.section, path) {
			appendDraft(&drafts, childDraft)
		}
	}
	flush()

	return drafts
}

func appendDraft(drafts *[]chapterDraft, draft chapterDraft) {
	if len(draft.blocks) == 0 {
		return
	}

	if len(*drafts) > 0 {
		last := &(*drafts)[len(*drafts)-1]
		if last.source != nil && last.source == draft.source {
			last.blocks = append(last.blocks, draft.blocks...)
			return
		}
	}

	*drafts = append(*drafts, draft)
}

func isVisibleTextBlock(name xml.Name) bool {
	if name.Space != Namespace {
		return false
	}

	switch name.Local {
	case "p", "subtitle", "v", "text-author", "date", "th", "td", "title":
		return true
	default:
		return false
	}
}

func normalizeSpace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
