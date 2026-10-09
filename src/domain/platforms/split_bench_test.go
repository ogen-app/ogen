package platforms

import (
	"strings"
	"testing"
)

// threadBody is a long plain-prose post with a little Markdown, the shape the
// thread auto-split usually sees.
func threadBody(chars int) string {
	const para = "Most teams publish on a schedule nobody remembers agreeing to. " +
		"The **one** habit that changes that is writing the brief first. " +
		"Then every post has a job, and every job has a deadline.\n\n"
	return strings.Repeat(para, chars/len(para)+1)[:chars]
}

func BenchmarkSplitThread2k(b *testing.B) {
	body := threadBody(2000)
	b.ReportAllocs()
	for b.Loop() {
		SplitThread(body, 280)
	}
}

func BenchmarkSplitThread10k(b *testing.B) {
	body := threadBody(10000)
	b.ReportAllocs()
	for b.Loop() {
		SplitThread(body, 280)
	}
}

func BenchmarkVisibleLen1k(b *testing.B) {
	body := threadBody(1000)
	b.ReportAllocs()
	for b.Loop() {
		VisibleLen(body)
	}
}
