package pdfprobe_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/infra/storage/pdfprobe"
)

// buildPDF synthesises a structurally-valid n-page PDF with correct
// xref offsets so pdfprobe.Probe succeeds.
func buildPDF(n int) []byte {
	if n < 1 {
		n = 1
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	offsets := make([]int, 0, 2+n)
	offsets = append(offsets, buf.Len())
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	offsets = append(offsets, buf.Len())
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [")
	for i := range n {
		if i > 0 {
			buf.WriteString(" ")
		}
		buf.WriteString(fmt.Sprintf("%d 0 R", 3+i))
	}
	buf.WriteString(fmt.Sprintf("] /Count %d >>\nendobj\n", n))

	for i := range n {
		offsets = append(offsets, buf.Len())
		buf.WriteString(fmt.Sprintf("%d 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>\nendobj\n", 3+i))
	}

	xrefStart := buf.Len()
	totalObjs := 2 + n
	buf.WriteString(fmt.Sprintf("xref\n0 %d\n", totalObjs+1))
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		buf.WriteString(fmt.Sprintf("%010d 00000 n \n", off))
	}

	buf.WriteString(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\n", totalObjs+1))
	buf.WriteString(fmt.Sprintf("startxref\n%d\n", xrefStart))
	buf.WriteString("%%EOF\n")
	return buf.Bytes()
}

func TestProbeSinglePage(t *testing.T) {
	data := buildPDF(1)
	res, raw, err := pdfprobe.Probe(bytes.NewReader(data), 10<<20)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.MIME != "application/pdf" {
		t.Errorf("mime = %q, want application/pdf", res.MIME)
	}
	if res.Extension != ".pdf" {
		t.Errorf("ext = %q, want .pdf", res.Extension)
	}
	if res.SHA256 == "" {
		t.Error("sha256 is empty")
	}
	if res.Size != int64(len(data)) {
		t.Errorf("size = %d, want %d", res.Size, len(data))
	}
	if !bytes.Equal(raw, data) {
		t.Error("raw bytes mismatch")
	}
}

func TestProbeMultiPage(t *testing.T) {
	data := buildPDF(7)
	res, raw, err := pdfprobe.Probe(bytes.NewReader(data), 10<<20)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Size != int64(len(data)) || !bytes.Equal(raw, data) {
		t.Errorf("probe did not round-trip a multi-page PDF")
	}
}

func TestProbeRejectsNonPDF(t *testing.T) {
	_, _, err := pdfprobe.Probe(strings.NewReader("not a pdf at all"), 1<<20)
	if err == nil {
		t.Fatal("expected error for non-PDF input")
	}
}

func TestProbeRejectsOversize(t *testing.T) {
	data := buildPDF(1)
	_, _, err := pdfprobe.Probe(bytes.NewReader(data), int64(len(data)-1))
	if err == nil {
		t.Fatal("expected error for oversize input")
	}
}

func TestProbeRejectsEmpty(t *testing.T) {
	_, _, err := pdfprobe.Probe(bytes.NewReader(nil), 1<<20)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}
