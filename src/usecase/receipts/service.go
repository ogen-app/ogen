// Package receipts turns a vendor receipt email into structured data for
// Harbor's finance ledger. Ogen keeps nothing: it fetches the email from
// Resend (or takes the copy Harbor stored), parses it, has the model read
// it, and hands everything back. Harbor stores the email, the extraction and
// its cost.
package receipts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/genkit/flows/receipt_extraction"
	"github.com/ogen-app/ogen/src/infra/email/inbound"
	"github.com/ogen-app/ogen/src/infra/email/resend"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Errors the gRPC layer maps to status codes.
var (
	// ErrNotFound: Resend no longer has the email (NotFound).
	ErrNotFound = errors.New("receipt email not found")
	// ErrInvalid: the MIME does not parse, or it is over the size cap
	// (InvalidArgument). Fetching it again will not help.
	ErrInvalid = errors.New("receipt email invalid")
	// ErrUnavailable: Resend or the model is down or not configured
	// (Unavailable); the caller retries.
	ErrUnavailable = errors.New("receipt processing unavailable")
)

// Timeout bounds one ProcessReceipt: the fetch and the model call.
const Timeout = 60 * time.Second

// Fetcher downloads a received email. *resend.Client satisfies it.
type Fetcher interface {
	GetReceivedRaw(ctx context.Context, id string, maxBytes int64) ([]byte, error)
}

// Extractor runs the receipt_extraction flow. Any error that is not the
// flow's own (no model key set, the call failed) reads as unavailable.
type Extractor interface {
	ExtractReceipt(ctx context.Context, req receipt_extraction.Request) (*receipt_extraction.Response, error)
}

// Result is one processed email.
type Result struct {
	// Raw is the email as received.
	Raw        []byte
	Message    *inbound.Message
	Extraction receipt_extraction.Extraction
	Usage      receipt_extraction.Usage
}

// Service processes receipts.
type Service struct {
	Fetcher   Fetcher
	Extractor Extractor
}

// Process fetches the email by its Resend id, or takes raw when the id is
// empty, and extracts it.
func (s *Service) Process(ctx context.Context, resendEmailID string, raw []byte) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if resendEmailID != "" {
		var err error
		if raw, err = s.fetch(ctx, resendEmailID); err != nil {
			return nil, err
		}
	}
	if len(raw) > inbound.MaxRawBytes {
		return nil, fmt.Errorf("%w: the email is over %d MiB", ErrInvalid, inbound.MaxRawBytes>>20)
	}
	msg, err := inbound.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	res := &Result{Raw: raw, Message: msg}
	if err := s.extract(ctx, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) fetch(ctx context.Context, id string) ([]byte, error) {
	if s.Fetcher == nil {
		return nil, fmt.Errorf("%w: no Resend client", ErrUnavailable)
	}
	raw, err := s.Fetcher.GetReceivedRaw(ctx, id, inbound.MaxRawBytes)
	switch {
	case err == nil:
		return raw, nil
	case errors.Is(err, resend.ErrNotFound):
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, resend.ErrTooLarge):
		return nil, fmt.Errorf("%w: the email is over %d MiB", ErrInvalid, inbound.MaxRawBytes>>20)
	default:
		return nil, fmt.Errorf("%w: fetch from Resend: %v", ErrUnavailable, err)
	}
}

// extract has the model read the email. An email the model cannot read
// (nothing to read, a refusal, an answer that is not JSON even after a
// repair) still succeeds, with an empty extraction whose notes say why:
// Harbor files it for an operator, and a retry would read it the same way.
func (s *Service) extract(ctx context.Context, res *Result) error {
	if s.Extractor == nil {
		return fmt.Errorf("%w: no extraction flow", ErrUnavailable)
	}
	m := res.Message
	req := receipt_extraction.Request{
		From: m.From, ForwardedFrom: m.ForwardedFrom, Subject: m.Subject,
		HTMLBody: m.HTMLBody, TextBody: m.TextBody,
	}
	if m.Date != nil {
		req.Date = m.Date.UTC().Format(time.RFC1123Z)
	}
	if a, ok := firstPDF(m.Attachments); ok {
		req.PDF, req.PDFName = a.Content, a.Filename
	}
	resp, err := s.Extractor.ExtractReceipt(ctx, req)
	// A failed reading still cost its calls: Harbor books them either way.
	if resp != nil {
		res.Usage = resp.Usage
	}
	if ve, ok := errors.AsType[*receipt_extraction.ValidationError](err); ok {
		res.Extraction = receipt_extraction.Extraction{ConfidenceNotes: unreadable(ve.Msg)}
		return nil
	}
	if ae, ok := errors.AsType[*receipt_extraction.AIError](err); ok && (ae.Refused || ae.Invalid) {
		slog.WarnContext(ctx, "receipt could not be read", logging.AttrComponent, "receipts", logging.AttrError, err)
		res.Extraction = receipt_extraction.Extraction{ConfidenceNotes: unreadable(ae.Msg)}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	res.Extraction = resp.Extraction
	return nil
}

func unreadable(why string) string {
	return "The receipt could not be read: " + strings.TrimSuffix(why, ".") + "."
}

func firstPDF(atts []inbound.Attachment) (inbound.Attachment, bool) {
	for _, a := range atts {
		if a.ContentType == "application/pdf" || strings.EqualFold(path.Ext(a.Filename), ".pdf") {
			return a, true
		}
	}
	return inbound.Attachment{}, false
}
