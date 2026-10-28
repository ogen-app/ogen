// Package receipt_extraction reads a forwarded vendor receipt or invoice and
// returns its figures for Harbor's finance ledger. It is one model call over
// the email's HTML body, else its text body, else its PDF attachment, and it
// reports its own cost: the call runs for an operator, outside any tenant, so
// the usage recorder never sees it.
package receipt_extraction

import "github.com/ogen-app/ogen/src/infra/vendors/llm"

// PromptVersion names the prompt that produced an extraction, so Harbor can
// tell which extractions a prompt change would redo.
const PromptVersion = "receipt-v1"

// Receipt kinds, as Harbor books them. KindUnknown is an email that is not a
// receipt or invoice.
const (
	KindUnknown    = ""
	KindInvoice    = "invoice"
	KindReceipt    = "receipt"
	KindPrepayment = "prepayment"
	KindRefund     = "refund"
	KindCreditNote = "credit_note"
)

// Sources: which part of the email the model read.
const (
	SourceHTML = "html"
	SourceText = "text"
	SourcePDF  = "pdf"
)

// Request is one received email, parsed.
type Request struct {
	From          string
	ForwardedFrom string
	Subject       string
	// Date is the email's Date header, as text; "" when missing.
	Date     string
	HTMLBody string
	TextBody string
	// PDF is the first PDF attachment, read only when the email has no
	// body.
	PDF     []byte
	PDFName string
}

// LineItem is one billed line. Figures are micros of the currency; nil
// when the document does not print them.
type LineItem struct {
	Description     string
	QuantityMicros  *int64
	UnitPriceMicros *int64
	AmountMicros    *int64
}

// Extraction is the model's reading of the document, every field as
// printed. Amounts are positive micros of Currency; nil when not printed.
type Extraction struct {
	Kind               string
	MerchantName       string
	DocumentNumber     string
	IssueDate          string
	ServicePeriodStart string
	ServicePeriodEnd   string
	Currency           string
	SubtotalMicros     *int64
	TaxMicros          *int64
	TotalMicros        *int64
	LineItems          []LineItem
	ConfidenceNotes    string
}

// Usage is what the extraction cost, across every call it took.
type Usage struct {
	ModelID       string
	PromptVersion string
	InputTokens   int64
	OutputTokens  int64
	// CostMicros is USD micros, priced like the usage pipeline prices a
	// tenant's calls.
	CostMicros int64
}

// Response is the extraction, the part of the email it read, and its cost.
type Response struct {
	Extraction Extraction
	Source     string
	Usage      Usage
}

// FlowConfig holds the flow's collaborators.
type FlowConfig struct {
	// Provider builds the call config and middleware.
	Provider *llm.Provider
}

// ValidationError: the email has nothing to read.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError is a failed model call.
type AIError struct {
	Msg string
	// Invalid: the answer was not valid JSON even after a repair turn.
	Invalid bool
	// Refused: the model declined.
	Refused bool
}

func (e *AIError) Error() string { return e.Msg }
