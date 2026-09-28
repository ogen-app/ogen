package jsonstream

import "strings"

// ObjectSplitter incrementally scans a streamed JSON array of objects and
// yields each top-level object's raw text as soon as its closing brace
// arrives. It tracks only string and brace state, so anything outside an
// object (the array brackets, commas, code fences, prose) is ignored.
type ObjectSplitter struct {
	buf      []byte
	depth    int
	inStr    bool
	escaped  bool
	objStart int // index of the current object's opening '{'; -1 outside one
}

// NewObjectSplitter returns an empty splitter.
func NewObjectSplitter() *ObjectSplitter {
	return &ObjectSplitter{objStart: -1}
}

// Push appends chunk and returns the objects completed by it, in order.
func (s *ObjectSplitter) Push(chunk string) []string {
	var complete []string
	for i := range len(chunk) {
		c := chunk[i]
		s.buf = append(s.buf, c)
		if s.stringByte(c) {
			continue
		}
		switch c {
		case '"':
			s.inStr = true
		case '{':
			if s.depth == 0 {
				s.objStart = len(s.buf) - 1
			}
			s.depth++
		case '}':
			s.depth--
			if s.depth == 0 && s.objStart >= 0 {
				complete = append(complete, string(s.buf[s.objStart:]))
				s.objStart = -1
			}
		}
	}
	return complete
}

// stringByte consumes c when it is inside a string literal (or is the byte
// after a backslash) and reports whether it did.
func (s *ObjectSplitter) stringByte(c byte) bool {
	if s.escaped {
		s.escaped = false
		return true
	}
	if !s.inStr {
		return false
	}
	switch c {
	case '\\':
		s.escaped = true
	case '"':
		s.inStr = false
	}
	return true
}

// StripFences removes a leading ```lang fence line and a trailing ``` from a
// blocking model response so a fenced JSON array still parses.
func StripFences(s string) string {
	text := strings.TrimSpace(s)
	if strings.HasPrefix(text, "```") {
		if _, after, found := strings.Cut(text, "\n"); found {
			text = after
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}
	return text
}
