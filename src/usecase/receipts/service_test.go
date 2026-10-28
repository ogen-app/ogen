package receipts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/genkit/flows/receipt_extraction"
	"github.com/ogen-app/ogen/src/infra/email/resend"
)

const rawReceipt = "From: Ops <ops@getogen.com>\r\nTo: receipts@in.getogen.com\r\nSubject: Fwd: Receipt\r\n" +
	"Date: Tue, 27 Oct 2026 09:15:00 +0000\r\nContent-Type: text/plain\r\n\r\n" +
	"Begin forwarded message:\r\n\r\nFrom: Resend <billing@resend.com>\r\n\r\nTotal $20.00\r\n"

type fakeFetcher struct {
	raw []byte
	err error
}

func (f fakeFetcher) GetReceivedRaw(context.Context, string, int64) ([]byte, error) {
	return f.raw, f.err
}

type fakeExtractor struct {
	got  receipt_extraction.Request
	resp *receipt_extraction.Response
	err  error
}

func (f *fakeExtractor) ExtractReceipt(_ context.Context, req receipt_extraction.Request) (*receipt_extraction.Response, error) {
	f.got = req
	return f.resp, f.err
}

func TestProcessByEmailIDAndRaw(t *testing.T) {
	total := int64(20_000_000)
	ex := &fakeExtractor{resp: &receipt_extraction.Response{
		Extraction: receipt_extraction.Extraction{Kind: receipt_extraction.KindReceipt, MerchantName: "Resend, Inc.", TotalMicros: &total},
		Usage:      receipt_extraction.Usage{ModelID: "claude-haiku-5-5", PromptVersion: "receipt-v1", CostMicros: 900},
	}}
	s := &Service{Fetcher: fakeFetcher{raw: []byte(rawReceipt)}, Extractor: ex}

	for name, call := range map[string]func() (*Result, error){
		"resend id": func() (*Result, error) { return s.Process(t.Context(), "em-1", nil) },
		"raw mime":  func() (*Result, error) { return s.Process(t.Context(), "", []byte(rawReceipt)) },
	} {
		res, err := call()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(res.Raw) != rawReceipt || res.Message.ForwardedFrom != "Resend <billing@resend.com>" ||
			res.Extraction.MerchantName != "Resend, Inc." || res.Usage.CostMicros != 900 {
			t.Fatalf("%s: result = %+v", name, res)
		}
		if ex.got.ForwardedFrom != "Resend <billing@resend.com>" || !strings.Contains(ex.got.TextBody, "Total $20.00") ||
			ex.got.Date != "Tue, 27 Oct 2026 09:15:00 +0000" {
			t.Fatalf("%s: extractor got %+v", name, ex.got)
		}
	}
}

func TestProcessErrors(t *testing.T) {
	ok := &fakeExtractor{resp: &receipt_extraction.Response{}}
	cases := []struct {
		name string
		s    *Service
		id   string
		raw  []byte
		want error
	}{
		{"gone at Resend", &Service{Fetcher: fakeFetcher{err: resend.ErrNotFound}, Extractor: ok}, "em-1", nil, ErrNotFound},
		{"too large at Resend", &Service{Fetcher: fakeFetcher{err: resend.ErrTooLarge}, Extractor: ok}, "em-1", nil, ErrInvalid},
		{"Resend down", &Service{Fetcher: fakeFetcher{err: errors.New("503")}, Extractor: ok}, "em-1", nil, ErrUnavailable},
		{"no Resend key", &Service{Fetcher: fakeFetcher{err: resend.ErrDisabled}, Extractor: ok}, "em-1", nil, ErrUnavailable},
		{"bad mime", &Service{Extractor: ok}, "", []byte("garbage"), ErrInvalid},
		{"model down", &Service{Extractor: &fakeExtractor{err: &receipt_extraction.AIError{Msg: "model call failed: 529"}}}, "", []byte(rawReceipt), ErrUnavailable},
		{"no model key", &Service{Extractor: &fakeExtractor{err: errors.New("anthropic api key not configured")}}, "", []byte(rawReceipt), ErrUnavailable},
	}
	for _, c := range cases {
		if _, err := c.s.Process(t.Context(), c.id, c.raw); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestProcessUnreadableStillSucceeds(t *testing.T) {
	for _, err := range []error{
		&receipt_extraction.ValidationError{Msg: "the email has no body and no PDF attachment to read"},
		&receipt_extraction.AIError{Msg: "model declined", Refused: true},
	} {
		s := &Service{Extractor: &fakeExtractor{err: err}}
		res, perr := s.Process(t.Context(), "", []byte(rawReceipt))
		if perr != nil || res.Extraction.Kind != receipt_extraction.KindUnknown || !strings.HasPrefix(res.Extraction.ConfidenceNotes, "The receipt could not be read") {
			t.Fatalf("%v: %+v %v", err, res, perr)
		}
	}
}
