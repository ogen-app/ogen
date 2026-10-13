package textgist

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	long := strings.Repeat("word ", 40) // 200 runes
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "  \n\t ", ""},
		{"trims and collapses", "  Reference   architectures\nfor RAG.  ", "Reference architectures for RAG."},
		{"strong", "**Reference** architectures for __RAG__.", "Reference architectures for RAG."},
		{"emphasis", "A *practical* guide to _fine-tuning_ at scale.", "A practical guide to fine-tuning at scale."},
		{"snake_case kept", "Tuning max_tokens and top_p for evals.", "Tuning max_tokens and top_p for evals."},
		{"link", "See [the playbook](https://x.test/p) for pricing.", "See the playbook for pricing."},
		{"image", "![chart](a.png) Revenue grew.", "chart Revenue grew."},
		{"code", "Calling `generate()` with tools.", "Calling generate() with tools."},
		{"cut on a word boundary keeps the word", long, strings.TrimSpace(strings.Repeat("word ", 32)) + "…"},
		{"cut mid-word backs off", "x" + long, "x" + strings.TrimSpace(strings.Repeat("word ", 31)) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clean(tt.in); got != tt.want {
				t.Errorf("Clean(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCleanCapsAtMaxRunes(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("ж", 400),                // no word boundary at all
		strings.Repeat("слово, ", 60),           // dangling comma at the cut
		strings.Repeat("a ", 79) + "abcdefghij", // boundary right at the cap
	} {
		got := Clean(in)
		if n := utf8.RuneCountInString(got); n > MaxRunes {
			t.Errorf("Clean(%.20q…) has %d runes, want <= %d", in, n, MaxRunes)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("Clean(%.20q…) = %q, want an ellipsis", in, got)
		}
		if strings.HasSuffix(got, ",…") || strings.HasSuffix(got, " …") {
			t.Errorf("Clean(%.20q…) = %q, dangling punctuation before the ellipsis", in, got)
		}
	}
}

func TestFromSummary(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "Reference architectures for RAG. Also covers evals.", "Reference architectures for RAG."},
		{
			"skips heading",
			"## Overview\n\nThe book maps GenAI patterns to Databricks. It has 118 pages.",
			"The book maps GenAI patterns to Databricks.",
		},
		{"heading line inside paragraph", "# Title\nThe call agreed on Q3 pricing. Next steps follow.", "The call agreed on Q3 pricing."},
		{"bold label line", "**Key takeaways:**\n\nPricing moves to seats. Usage stays free.", "Pricing moves to seats."},
		{"bold lead kept", "**Bottom line:** pricing moves to seats. More later.", "Bottom line: pricing moves to seats."},
		{"only bullets", "## Points\n\n- one\n- two\n\n1. three\n* four", ""},
		{"skips list then prose", "- a bullet\n- another\n\nThen the prose starts here. More.", "Then the prose starts here."},
		{"quote and table skipped", "> quoted\n\n| a | b |\n\nReal prose. More.", "Real prose."},
		{"abbreviations", "Covers tools, e.g. vector stores, i.e. the RAG layer. Then more.", "Covers tools, e.g. vector stores, i.e. the RAG layer."},
		{"initials", "Sales in the U.S. market doubled. Then more.", "Sales in the U.S. market doubled."},
		{"decimal", "Version 3.5 ships faster evals. Then more.", "Version 3.5 ships faster evals."},
		{"question", "Why do RAG systems fail? Mostly retrieval.", "Why do RAG systems fail?"},
		{"closing quote", `He said "ship it." Then left.`, `He said "ship it."`},
		{"no terminator", "A sentence that the 800-char read cut off before its end", "A sentence that the 800-char read cut off before its end"},
		{"non-english", "Der Bericht zeigt, wie RAG skaliert. Mehr folgt.", "Der Bericht zeigt, wie RAG skaliert."},
		{"cjk", "本書はRAGの設計を解説する。評価も扱う。", "本書はRAGの設計を解説する。"},
		{"crlf", "## Overview\r\n\r\nWindows line endings work. Yes.", "Windows line endings work."},
		{"markdown stripped", "A **practical** guide to [RAG](https://x.test). More.", "A practical guide to RAG."},
		{"terminator before non-ascii", "Costs fell 3.5%.—Then a dash. More.", "Costs fell 3.5%.—Then a dash."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromSummary(tt.in); got != tt.want {
				t.Errorf("FromSummary(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFromSummaryCapsLongSentence(t *testing.T) {
	got := FromSummary("## H\n\n" + strings.Repeat("long ", 60) + "end. Next.")
	if n := utf8.RuneCountInString(got); n > MaxRunes || !strings.HasSuffix(got, "…") {
		t.Fatalf("FromSummary = %q (%d runes), want <= %d with an ellipsis", got, n, MaxRunes)
	}
}
