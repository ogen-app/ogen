package flows

import "strings"

const (
	// MaxEmbedChars caps the characters fed to Gemini Embedding 2 per chunk.
	// At ~3.5 chars/token this is ~1,700 tokens — comfortably inside
	// Gemini's 8192-token input limit, so chunks never need truncation while
	// staying small enough for sharp, well-scoped embeddings.
	MaxEmbedChars = 6000
	// ChunkTarget is the target size for each chunk when splitting is needed.
	ChunkTarget = 5500
	// ChunkOverlap is the number of characters carried from the end of one
	// chunk into the start of the next to preserve context continuity.
	ChunkOverlap = 500
)

// ChunkText splits text into chunks suitable for Gemini Embedding 2.
//
//   - len(text) <= MaxEmbedChars → returns [text] (single chunk, no split)
//   - otherwise → paragraph-aware split: accumulates paragraphs up to
//     ChunkTarget chars, then carries the last ChunkOverlap chars of that
//     chunk into the next one as overlap.
func ChunkText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) <= MaxEmbedChars {
		return []string{text}
	}

	paragraphs := strings.Split(text, "\n\n")

	var chunks []string
	var current strings.Builder

	flush := func() {
		chunk := strings.TrimSpace(current.String())
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		// If a single paragraph is itself larger than ChunkTarget, split it
		// at word boundaries so it doesn't blow past MaxEmbedChars on its own.
		subParas := splitLargeParagraph(para)

		for _, sub := range subParas {
			// If adding this sub-paragraph would exceed the target, flush and
			// start a new chunk with the overlap tail of the previous one.
			if current.Len() > 0 && current.Len()+len(sub)+2 > ChunkTarget {
				flush()

				// Carry overlap from the tail of the flushed chunk.
				prev := current.String()
				current.Reset()
				if len(prev) > ChunkOverlap {
					current.WriteString(prev[len(prev)-ChunkOverlap:])
					current.WriteString("\n\n")
				}
			}

			if current.Len() > 0 {
				current.WriteString("\n\n")
			}
			current.WriteString(sub)
		}
	}

	flush()

	// Guard: if we somehow produced no chunks, return the original text.
	if len(chunks) == 0 {
		return []string{text}
	}
	return chunks
}

// splitLargeParagraph splits a paragraph that exceeds ChunkTarget into smaller
// pieces at word boundaries. Used as a fallback when a single block of text
// (no paragraph separators) is larger than the chunk target.
func splitLargeParagraph(para string) []string {
	if len(para) <= ChunkTarget {
		return []string{para}
	}
	var parts []string
	remaining := para
	for len(remaining) > ChunkTarget {
		boundary := ChunkTarget
		// Walk back to the nearest space to avoid splitting mid-word.
		for boundary > 0 && remaining[boundary] != ' ' {
			boundary--
		}
		if boundary == 0 {
			boundary = ChunkTarget // no space found — hard split
		}
		parts = append(parts, remaining[:boundary])
		remaining = remaining[boundary:]
		if len(remaining) > 0 && remaining[0] == ' ' {
			remaining = remaining[1:]
		}
	}
	if len(remaining) > 0 {
		parts = append(parts, remaining)
	}
	return parts
}

// EstimateTokens returns a rough token count for text using the 3.5 chars/token
// heuristic common for byte-pair encoded models.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	t := len(text) / 4
	if t == 0 {
		return 1
	}
	return t
}
