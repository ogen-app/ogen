// Package pdfprobe is the PDF sibling to imageprobe. It
// validates a PDF upload by sniffing the magic bytes (never trusting
// the client's declared Content-Type), checking the size, and computing
// a SHA-256 checksum.
//
// Page counting moved to the pdf-service microservice; the
// upload handler obtains page count (and a thumbnail) from it.
package pdfprobe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// MIME is the canonical PDF media type.
const MIME = "application/pdf"

// Extension is the on-disk / S3-key extension for PDF attachments.
const Extension = ".pdf"

// pdfMagic is the 5-byte signature that opens every conforming PDF.
var pdfMagic = []byte("%PDF-")

// Result mirrors imageprobe.Result for shared call-site shape. Page count is
// not included — it comes from pdf-service.
type Result struct {
	MIME      string
	Extension string
	SHA256    string
	Size      int64
}

// ErrUnsupportedMIME is returned when the magic bytes do not match a PDF.
var ErrUnsupportedMIME = errors.New("pdfprobe: unsupported media type")

// Probe reads r in full (capped at limit), verifies the PDF magic
// bytes, and computes SHA-256. It returns the buffered bytes so the
// caller can stream them to S3 without re-reading r.
func Probe(r io.Reader, limit int64) (*Result, []byte, error) {
	buf := &bytes.Buffer{}
	n, err := io.Copy(buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("pdfprobe: read: %w", err)
	}
	if n > limit {
		return nil, nil, fmt.Errorf("pdfprobe: file exceeds limit of %d bytes", limit)
	}
	data := buf.Bytes()
	if len(data) == 0 {
		return nil, nil, errors.New("pdfprobe: empty file")
	}
	if !bytes.HasPrefix(data, pdfMagic) {
		return nil, nil, fmt.Errorf("%w: not a PDF", ErrUnsupportedMIME)
	}

	sum := sha256.Sum256(data)
	return &Result{
		MIME:      MIME,
		Extension: Extension,
		SHA256:    hex.EncodeToString(sum[:]),
		Size:      n,
	}, data, nil
}
