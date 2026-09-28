package jsonstream

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Scanner is an incremental parser for a single top-level JSON
// object. It serves two purposes:
//
//   - During a stream, it invokes onDelta with decoded fragments of watched
//     top-level string keys so the UI can preview text as it arrives.
//
//   - After the stream finishes, Values() returns a map of every top-level
//     key's parsed value — strings decoded, literals (true/false/null and
//     numbers) coerced to their Go equivalents. This is the authoritative
//     extraction path; it bypasses encoding/json entirely and therefore
//     tolerates trailing commas, missing separators, preamble/postamble
//     prose, literal newlines inside strings, and truncation. Only keys
//     whose values the scanner actually observed are present in the map.
//
// The scanner is forgiving of preamble (markdown fences, prose) before the
// opening `{` and of trailing text after the closing `}`.
type Scanner struct {
	watched map[string]bool
	onDelta func(key, delta string)

	state    scannerState
	esc      escState
	keyBuf   []byte
	hexBuf   []byte
	pendHigh rune // high surrogate awaiting its low pair
	curKey   string
	watching bool

	nestedDepth int

	// deltaBuf accumulates decoded bytes of the currently-streaming watched
	// string value. It is flushed at the end of every Push call.
	deltaBuf []byte
	// carry holds trailing bytes of an incomplete UTF-8 sequence that must
	// be prepended to deltaBuf before the next flush.
	carry []byte

	// fullText accumulates the raw input bytes — useful for debugging but
	// not used by the authoritative extraction (which goes via accumulators).
	fullText []byte

	// accumulators stores per-key complete values. Populated as values
	// stream in (strings decoded, literals captured raw); snapshotted by
	// Values() into a typed map[string]any.
	accumulators map[string]*valueAcc
}

type valueKind int

const (
	valKindString valueKind = iota
	valKindLiteral
)

type valueAcc struct {
	kind valueKind
	buf  []byte
}

type scannerState int

const (
	stPreamble scannerState = iota
	stTop                   // between keys at the top level
	stInKey
	stAfterKey
	stAwaitValue
	stInString
	stCollectLiteral // inside a non-string value (bool / number / null) — accumulate bytes
	stAfterValue     // value just closed, looking for "," or "}"
	stInNested
	stDone
)

type escState int

const (
	escNone escState = iota
	escBackslash
	escUnicode
)

// New returns a scanner that calls onDelta whenever new
// decoded bytes of a watched string key are available. watched may be nil
// if callers only need Values() (no streaming preview).
func New(watched []string, onDelta func(key, delta string)) *Scanner {
	m := make(map[string]bool, len(watched))
	for _, k := range watched {
		m[k] = true
	}
	return &Scanner{
		watched:      m,
		onDelta:      onDelta,
		accumulators: make(map[string]*valueAcc),
	}
}

// Push feeds a chunk of raw text from the model stream. It appends the
// chunk to the internal raw buffer, advances the state machine, and flushes
// any pending decoded delta at the end.
func (s *Scanner) Push(chunk string) {
	s.fullText = append(s.fullText, chunk...)
	for i := 0; i < len(chunk); i++ {
		s.step(chunk[i])
	}
	s.flushDelta()
}

// FullText returns every byte pushed into the scanner so far.
func (s *Scanner) FullText() string {
	return string(s.fullText)
}

// Values snapshots the scanner's per-key accumulators into a typed map.
// Strings are decoded; "true"/"false"/"null" become bool/nil; other
// literals are parsed as float64 or returned as the raw trimmed string
// on parse failure. Keys that the scanner did not see (including those
// whose value was a nested object/array, which is skipped) are absent.
func (s *Scanner) Values() map[string]any {
	out := make(map[string]any, len(s.accumulators))
	for key, acc := range s.accumulators {
		if acc == nil {
			continue
		}
		switch acc.kind {
		case valKindString:
			out[key] = string(acc.buf)
		case valKindLiteral:
			raw := strings.TrimSpace(string(acc.buf))
			switch raw {
			case "true":
				out[key] = true
			case "false":
				out[key] = false
			case "null":
				out[key] = nil
			default:
				if n, err := strconv.ParseFloat(raw, 64); err == nil {
					out[key] = n
				} else {
					out[key] = raw
				}
			}
		}
	}
	return out
}

func (s *Scanner) step(c byte) {
	switch s.state {
	case stPreamble:
		if c == '{' {
			s.state = stTop
		}
	case stTop:
		s.topByte(c)
	case stInKey:
		s.keyByte(c)
	case stAfterKey:
		if c == ':' {
			s.state = stAwaitValue
		}
	case stAwaitValue:
		s.awaitValueByte(c)
	case stInString:
		s.stringByte(c)
	case stCollectLiteral:
		s.literalByte(c)
	case stAfterValue:
		s.separatorByte(c)
	case stInNested:
		s.nestedByte(c)
	case stDone:
		// ignore everything after top-level close
	}
}

func (s *Scanner) topByte(c byte) {
	switch c {
	case '"':
		s.beginKey()
	case '}':
		s.state = stDone
	}
}

// beginKey starts parsing a key after its opening quote.
func (s *Scanner) beginKey() {
	s.keyBuf = s.keyBuf[:0]
	s.esc = escNone
	s.state = stInKey
}

// keyByte parses key bytes. A backslash only protects the next byte; \uXXXX
// is not decoded because keys never need it in practice.
func (s *Scanner) keyByte(c byte) {
	if s.esc == escBackslash {
		s.keyBuf = append(s.keyBuf, c)
		s.esc = escNone
		return
	}
	switch c {
	case '\\':
		s.esc = escBackslash
	case '"':
		s.curKey = string(s.keyBuf)
		s.watching = s.watched[s.curKey]
		s.state = stAfterKey
	default:
		s.keyBuf = append(s.keyBuf, c)
	}
}

func (s *Scanner) awaitValueByte(c byte) {
	switch c {
	case ' ', '\t', '\n', '\r':
	case '"':
		s.esc = escNone
		s.hexBuf = s.hexBuf[:0]
		s.pendHigh = 0
		s.resetAccumulator(s.curKey, valKindString)
		s.state = stInString
	case '{', '[':
		// Nested values are not extracted but are skipped cleanly so the
		// following keys still parse.
		s.nestedDepth = 1
		s.state = stInNested
	default:
		s.resetAccumulator(s.curKey, valKindLiteral)
		s.appendToAccumulator(c)
		s.state = stCollectLiteral
	}
}

// literalByte accumulates a bool / number / null literal. Whitespace ends
// the literal and hands over to separatorByte for the next byte.
func (s *Scanner) literalByte(c byte) {
	switch c {
	case ',', '}', '"':
		s.separatorByte(c)
	case ' ', '\t', '\n', '\r':
		s.state = stAfterValue
	default:
		s.appendToAccumulator(c)
	}
}

// separatorByte handles the bytes after a complete value. A quote where a
// comma was expected starts the next key, recovering from a missing comma.
func (s *Scanner) separatorByte(c byte) {
	switch c {
	case ',':
		s.resetKey()
		s.state = stTop
	case '}':
		s.state = stDone
	case '"':
		s.resetKey()
		s.beginKey()
	}
}

func (s *Scanner) nestedByte(c byte) {
	switch c {
	case '{', '[':
		s.nestedDepth++
	case '}', ']':
		s.nestedDepth--
		if s.nestedDepth == 0 {
			s.resetKey()
			s.state = stTop
		}
	}
}

func (s *Scanner) resetKey() {
	s.curKey = ""
	s.watching = false
}

// resetAccumulator initialises or overwrites the accumulator for key.
// Overwrite semantics give "last-wins" behaviour for duplicate keys.
func (s *Scanner) resetAccumulator(key string, kind valueKind) {
	if key == "" {
		return
	}
	s.accumulators[key] = &valueAcc{kind: kind}
}

// appendToAccumulator writes a raw byte to the current key's accumulator.
// Used by the literal-collection path.
func (s *Scanner) appendToAccumulator(c byte) {
	if s.curKey == "" {
		return
	}
	if acc := s.accumulators[s.curKey]; acc != nil {
		acc.buf = append(acc.buf, c)
	}
}

// simpleEscapes maps the byte after a backslash to its decoded byte for the
// single-character JSON escapes; zero marks anything else.
var simpleEscapes = [256]byte{
	'"':  '"',
	'\\': '\\',
	'/':  '/',
	'n':  '\n',
	't':  '\t',
	'r':  '\r',
	'b':  '\b',
	'f':  '\f',
}

func (s *Scanner) stringByte(c byte) {
	switch s.esc {
	case escNone:
		s.plainStringByte(c)
	case escBackslash:
		s.escapeByte(c)
	case escUnicode:
		s.hexBuf = append(s.hexBuf, c)
		if len(s.hexBuf) == 4 {
			s.decodeUnicodeEscape()
			s.hexBuf = s.hexBuf[:0]
			s.esc = escNone
		}
	}
}

func (s *Scanner) plainStringByte(c byte) {
	switch c {
	case '\\':
		s.esc = escBackslash
	case '"':
		// Leave stInString before flushing so flushDelta emits any dangling
		// partial-UTF-8 carry instead of retaining it for a next chunk.
		s.state = stAfterValue
		s.flushDelta()
		s.resetKey()
		s.carry = s.carry[:0]
	default:
		s.appendByte(c)
	}
}

// escapeByte decodes the byte after a backslash. An unknown escape emits the
// byte verbatim so nothing is silently lost.
func (s *Scanner) escapeByte(c byte) {
	if c == 'u' {
		s.hexBuf = s.hexBuf[:0]
		s.esc = escUnicode
		return
	}
	if d := simpleEscapes[c]; d != 0 {
		c = d
	}
	s.appendByte(c)
	s.esc = escNone
}

// decodeUnicodeEscape decodes the four hex digits in hexBuf, pairing UTF-16
// surrogates: a lone low surrogate is dropped, and a pending high surrogate
// followed by a non-surrogate is emitted as-is first. Invalid hex is ignored.
func (s *Scanner) decodeUnicodeEscape() {
	v, err := strconv.ParseUint(string(s.hexBuf), 16, 32)
	if err != nil {
		return
	}
	r := rune(v)
	switch {
	case r >= 0xD800 && r <= 0xDBFF:
		s.pendHigh = r
	case r >= 0xDC00 && r <= 0xDFFF:
		if s.pendHigh != 0 {
			s.appendRune(utf16.DecodeRune(s.pendHigh, r))
			s.pendHigh = 0
		}
	default:
		if s.pendHigh != 0 {
			s.appendRune(s.pendHigh)
			s.pendHigh = 0
		}
		s.appendRune(r)
	}
}

// appendByte writes a decoded byte to (a) the current key's accumulator
// and (b) — only for watched keys — the streaming delta buffer.
func (s *Scanner) appendByte(c byte) {
	if s.curKey != "" {
		if acc := s.accumulators[s.curKey]; acc != nil {
			acc.buf = append(acc.buf, c)
		}
	}
	if !s.watching {
		return
	}
	s.deltaBuf = append(s.deltaBuf, c)
}

// appendRune writes a decoded rune's UTF-8 bytes to accumulator and
// (for watched keys) delta buffer.
func (s *Scanner) appendRune(r rune) {
	var buf [4]byte
	n := utf8.EncodeRune(buf[:], r)
	if s.curKey != "" {
		if acc := s.accumulators[s.curKey]; acc != nil {
			acc.buf = append(acc.buf, buf[:n]...)
		}
	}
	if !s.watching {
		return
	}
	s.deltaBuf = append(s.deltaBuf, buf[:n]...)
}

// flushDelta emits the accumulated decoded bytes for the current key,
// carrying any trailing incomplete UTF-8 bytes over to the next call.
func (s *Scanner) flushDelta() {
	// Nothing to do only when there are neither fresh bytes nor a carried
	// partial-UTF-8 tail. When the string has just ended, deltaBuf can be empty
	// while carry still holds trailing bytes that must be emitted below.
	if len(s.deltaBuf) == 0 && len(s.carry) == 0 {
		return
	}
	// Prepend any carried bytes from the previous flush.
	if len(s.carry) > 0 {
		merged := make([]byte, 0, len(s.carry)+len(s.deltaBuf))
		merged = append(merged, s.carry...)
		merged = append(merged, s.deltaBuf...)
		s.deltaBuf = merged
		s.carry = s.carry[:0]
	}
	full, rest := trimIncompleteUTF8(s.deltaBuf)
	if len(full) > 0 && s.onDelta != nil && s.curKey != "" {
		s.onDelta(s.curKey, string(full))
	}
	// If the string ended (state transitioned past stInString), there's no
	// next chunk for this key — emit the trailing bytes as-is and drop.
	if s.state != stInString && len(rest) > 0 && s.onDelta != nil && s.curKey != "" {
		s.onDelta(s.curKey, string(rest))
		rest = nil
	}
	s.carry = append(s.carry[:0], rest...)
	s.deltaBuf = s.deltaBuf[:0]
}

// trimIncompleteUTF8 returns the prefix of b that is a complete UTF-8
// sequence and the trailing bytes (if any) that form a partial sequence.
func trimIncompleteUTF8(b []byte) (full, rest []byte) {
	if len(b) == 0 {
		return b, nil
	}
	// Walk back at most 3 bytes to find the lead byte of the final rune.
	for i := len(b) - 1; i >= 0 && i >= len(b)-4; i-- {
		c := b[i]
		if c < 0x80 {
			// ASCII — prior bytes are complete, nothing to carry.
			return b, nil
		}
		if c&0xC0 == 0x80 {
			// continuation byte — keep walking back
			continue
		}
		// lead byte
		var need int
		switch {
		case c&0xE0 == 0xC0:
			need = 2
		case c&0xF0 == 0xE0:
			need = 3
		case c&0xF8 == 0xF0:
			need = 4
		default:
			return b, nil
		}
		have := len(b) - i
		if have >= need {
			return b, nil
		}
		return b[:i], b[i:]
	}
	return b, nil
}
