package flows_test

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/genkit/flows"
)

func TestChunkText_ShortText(t *testing.T) {
	text := "Short asset content."
	chunks := flows.ChunkText(text)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Fatalf("expected chunk to equal input, got %q", chunks[0])
	}
}

func TestChunkText_ExactlyAtLimit(t *testing.T) {
	text := strings.Repeat("a", flows.MaxEmbedChars)
	chunks := flows.ChunkText(text)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk at limit, got %d", len(chunks))
	}
}

func TestChunkText_LongTextProducesMultipleChunks(t *testing.T) {
	// Build a text clearly over MaxEmbedChars using distinct paragraphs.
	var sb strings.Builder
	for range 20 {
		sb.WriteString(strings.Repeat("word ", 100)) // ~500 chars per para
		sb.WriteString("\n\n")
	}
	text := sb.String() // ~10,000 chars

	chunks := flows.ChunkText(text)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks for %d-char text, got %d", len(text), len(chunks))
	}
	for i, c := range chunks {
		if len(c) > flows.ChunkTarget+flows.ChunkOverlap+200 {
			t.Errorf("chunk %d too large: %d chars", i, len(c))
		}
	}
}

func TestChunkText_OverlapPresent(t *testing.T) {
	// Two large paragraphs — overlap from para1 should appear at start of chunk2.
	para1 := strings.Repeat("X", flows.ChunkTarget-100)
	para2 := strings.Repeat("Y", flows.ChunkTarget)
	text := para1 + "\n\n" + para2

	chunks := flows.ChunkText(text)
	if len(chunks) < 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	// chunk[1] should start with the overlap tail of chunk[0].
	tail := chunks[0][len(chunks[0])-flows.ChunkOverlap:]
	if !strings.Contains(chunks[1], strings.TrimSpace(tail)) {
		t.Errorf("overlap not found in second chunk\ntail: %q\nchunk[1] start: %q",
			tail[:50], chunks[1][:min(50, len(chunks[1]))])
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"hello", 1},
		{strings.Repeat("a", 400), 100},
	}
	for _, tc := range cases {
		got := flows.EstimateTokens(tc.text)
		if got != tc.want {
			t.Errorf("EstimateTokens(%d chars) = %d, want %d", len(tc.text), got, tc.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
