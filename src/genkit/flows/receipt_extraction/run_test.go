package receipt_extraction

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
)

// stripeReply is how the model answers for a Railway receipt sent by Stripe.
const stripeReply = "```json\n" + `{
  "kind": "receipt", "merchantName": "Railway Corporation", "documentNumber": "2481-0012",
  "issueDate": "2026-10-26", "servicePeriodStart": "2026-09-26", "servicePeriodEnd": "2026-10-26",
  "currency": "usd", "subtotal": "20.00", "tax": "0.00", "total": "$20.00",
  "lineItems": [
    {"description": "Hobby plan", "quantity": "1", "unitPrice": "5.00", "amount": "5.00"},
    {"description": "Usage", "quantity": "", "unitPrice": null, "amount": 15}
  ],
  "confidenceNotes": ""
}` + "\n```"

func newTestRunner(t *testing.T, replies ...string) (*runner, *[]modelCall) {
	t.Helper()
	tpl, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	var calls []modelCall
	gen := func(_ context.Context, call modelCall) (modelReply, error) {
		calls = append(calls, call)
		if len(calls) > len(replies) {
			return modelReply{}, errors.New("unexpected call")
		}
		return modelReply{Text: replies[len(calls)-1], InputTokens: 1000, OutputTokens: 200, CostMicros: 1500}, nil
	}
	resolve := func(context.Context, string, string) modelconfig.Resolved {
		return modelconfig.Resolved{Ref: "anthropic/claude-haiku-5-5", Vendor: "anthropic", Model: "claude-haiku-5-5"}
	}
	return &runner{tpl: tpl, gen: gen, resolve: resolve}, &calls
}

func TestExtractFromHTML(t *testing.T) {
	r, calls := newTestRunner(t, stripeReply)
	resp, err := r.extract(t.Context(), Request{
		From: "Ops <ops@getogen.com>", ForwardedFrom: "Railway <invoice+statements@railway.app>", Subject: "Fwd: Your receipt",
		HTMLBody: `<html><head><style>.x{}</style></head><body><table class="t"><tr><td style="a">Amount paid</td><td>$20.00</td></tr></table></body></html>`,
		TextBody: "ignored when there is HTML", PDF: []byte("%PDF"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].PDF != nil {
		t.Fatalf("calls = %+v", *calls)
	}
	prompt := (*calls)[0].Prompt
	for _, want := range []string{"Forwarded from: Railway", "Body (html):", "<table><tr><td>Amount paid</td><td>$20.00</td></tr></table>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "style") || strings.Contains(prompt, "ignored when") {
		t.Errorf("prompt kept noise:\n%s", prompt)
	}
	e := resp.Extraction
	if e.Kind != KindReceipt || e.MerchantName != "Railway Corporation" || e.DocumentNumber != "2481-0012" || e.Currency != "USD" ||
		e.IssueDate != "2026-10-26" || e.ServicePeriodEnd != "2026-10-26" || *e.TotalMicros != 20_000_000 || *e.TaxMicros != 0 {
		t.Fatalf("extraction = %+v", e)
	}
	if len(e.LineItems) != 2 || *e.LineItems[0].UnitPriceMicros != 5_000_000 || e.LineItems[1].QuantityMicros != nil ||
		e.LineItems[1].UnitPriceMicros != nil || *e.LineItems[1].AmountMicros != 15_000_000 {
		t.Fatalf("lines = %+v", e.LineItems)
	}
	if resp.Source != SourceHTML || resp.Usage != (Usage{ModelID: "claude-haiku-5-5", PromptVersion: PromptVersion, InputTokens: 1000, OutputTokens: 200, CostMicros: 1500}) {
		t.Fatalf("source = %s, usage = %+v", resp.Source, resp.Usage)
	}
}

func TestExtractFallsBackToTextThenPDF(t *testing.T) {
	r, calls := newTestRunner(t, stripeReply, stripeReply)
	if _, err := r.extract(t.Context(), Request{Subject: "s", TextBody: "Total: $20.00"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*calls)[0].Prompt, "Body (text):\nTotal: $20.00") {
		t.Fatalf("text prompt:\n%s", (*calls)[0].Prompt)
	}
	resp, err := r.extract(t.Context(), Request{Subject: "s", PDF: []byte("%PDF"), PDFName: "Invoice.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if call := (*calls)[1]; string(call.PDF) != "%PDF" || !strings.Contains(call.Prompt, "attached PDF (Invoice.pdf)") || resp.Source != SourcePDF {
		t.Fatalf("pdf call = %+v", call)
	}

	var ve *ValidationError
	if _, err := r.extract(t.Context(), Request{Subject: "nothing"}); !errors.As(err, &ve) {
		t.Fatalf("empty email: %v", err)
	}
}

func TestExtractRepairsBadJSONAndSumsUsage(t *testing.T) {
	r, calls := newTestRunner(t, "Sure! Here it is: {kind: receipt", stripeReply)
	resp, err := r.extract(t.Context(), Request{TextBody: "Total $20"})
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || !strings.Contains((*calls)[1].Prompt, "could not be parsed") {
		t.Fatalf("calls = %d", len(*calls))
	}
	if resp.Usage.InputTokens != 2000 || resp.Usage.CostMicros != 3000 {
		t.Fatalf("usage = %+v, want both calls", resp.Usage)
	}

	r, _ = newTestRunner(t, "nope", "still nope")
	var ae *AIError
	resp, err = r.extract(t.Context(), Request{TextBody: "x"})
	if !errors.As(err, &ae) || !ae.Invalid {
		t.Fatalf("err = %v", err)
	}
	if resp == nil || resp.Usage.CostMicros != 3000 || resp.Usage.ModelID != "claude-haiku-5-5" {
		t.Fatalf("failed reading lost its usage: %+v", resp)
	}
}

func TestExtractionDropsWhatDoesNotHoldUp(t *testing.T) {
	a := answer{Kind: "statement", Currency: "dollars", IssueDate: "26/10/2026", Total: "lots", ConfidenceNotes: "Faded scan."}
	e := a.extraction()
	if e.Kind != KindUnknown || e.Currency != "" || e.IssueDate != "" || e.TotalMicros != nil {
		t.Fatalf("extraction = %+v", e)
	}
	for _, want := range []string{"Faded scan.", "not an ISO 4217", "not a date", "not a figure"} {
		if !strings.Contains(e.ConfidenceNotes, want) {
			t.Errorf("notes %q lack %q", e.ConfidenceNotes, want)
		}
	}
}

func TestMicros(t *testing.T) {
	cases := map[string]int64{
		"20.00": 20_000_000, "20": 20_000_000, "1,234.56": 1_234_560_000, "1.234,56": 1_234_560_000, "12,50": 12_500_000, "€ 9.99": 9_990_000,
		"-5.00": 5_000_000, "(3.10)": 3_100_000, "0.0000015": 1, "1.5": 1_500_000, ".5": 500_000,
	}
	for in, want := range cases {
		got, ok, err := micros(in)
		if err != nil || !ok || got != want {
			t.Errorf("micros(%q) = %d %v %v, want %d", in, got, ok, err, want)
		}
	}
	if _, ok, err := micros(""); ok || err != nil {
		t.Error("empty figure read as a value")
	}
	if _, _, err := micros("abc"); err == nil {
		t.Error("abc read as a figure")
	}
}
