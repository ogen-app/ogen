package content_plan

import (
	"strings"
	"testing"
)

func TestTrimBody(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "short body unchanged",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "empty body unchanged",
			input: "",
			want:  "",
		},
		{
			name:  "exactly 500 runes unchanged",
			input: strings.Repeat("a", 500),
			want:  strings.Repeat("a", 500),
		},
		{
			name:  "501 runes truncated to 500",
			input: strings.Repeat("a", 501),
			want:  strings.Repeat("a", 500),
		},
		{
			name:  "multibyte unicode truncated by rune count not byte count",
			input: strings.Repeat("é", 501), // é is 2 bytes; must cut at 500 runes, not 500 bytes
			want:  strings.Repeat("é", 500),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trimBody(tt.input)
			if got != tt.want {
				t.Errorf("trimBody: got %d runes, want %d", len([]rune(got)), len([]rune(tt.want)))
			}
		})
	}
}

func TestParseAndTrimPost(t *testing.T) {
	t.Run("valid JSON returns post", func(t *testing.T) {
		raw := `{"title":"Hello","body":"World","platformId":"abc","contentType":"text-post","publishDate":"2026-05-01","toneNotes":"","assetRefs":[]}`
		post, ok := parseAndTrimPost(raw)
		if !ok {
			t.Fatal("expected ok=true, got false")
		}
		if post.Title != "Hello" {
			t.Errorf("title: got %q, want %q", post.Title, "Hello")
		}
		if post.Body != "World" {
			t.Errorf("body: got %q, want %q", post.Body, "World")
		}
	})

	t.Run("body over 500 runes is truncated", func(t *testing.T) {
		longBody := strings.Repeat("x", 600)
		raw := `{"title":"T","body":"` + longBody + `"}`
		post, ok := parseAndTrimPost(raw)
		if !ok {
			t.Fatal("expected ok=true, got false")
		}
		if got := len([]rune(post.Body)); got != 500 {
			t.Errorf("body rune count: got %d, want 500", got)
		}
	})

	t.Run("malformed JSON returns false", func(t *testing.T) {
		_, ok := parseAndTrimPost(`{"title": bad json`)
		if ok {
			t.Error("expected ok=false for malformed JSON, got true")
		}
	})

	t.Run("empty string returns false", func(t *testing.T) {
		_, ok := parseAndTrimPost("")
		if ok {
			t.Error("expected ok=false for empty input, got true")
		}
	})
}

func TestTailOf(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"short string returned as-is", "hello", 10, "hello"},
		{"exact length returned as-is", "hello", 5, "hello"},
		{"longer string gets prefix", "hello world", 5, "...world"},
		{"empty string", "", 5, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tailOf(tt.s, tt.n)
			if got != tt.want {
				t.Errorf("tailOf(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
		})
	}
}
