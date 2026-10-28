package receipt_extraction

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/genkit/jsonstream"
)

// answer is the JSON the prompt asks for. Figures are decimal strings, so
// the model copies them as printed and the conversion to micros is exact.
type answer struct {
	Kind               string       `json:"kind"`
	MerchantName       string       `json:"merchantName"`
	DocumentNumber     string       `json:"documentNumber"`
	IssueDate          string       `json:"issueDate"`
	ServicePeriodStart string       `json:"servicePeriodStart"`
	ServicePeriodEnd   string       `json:"servicePeriodEnd"`
	Currency           string       `json:"currency"`
	Subtotal           flexDecimal  `json:"subtotal"`
	Tax                flexDecimal  `json:"tax"`
	Total              flexDecimal  `json:"total"`
	LineItems          []answerLine `json:"lineItems"`
	ConfidenceNotes    string       `json:"confidenceNotes"`
}

type answerLine struct {
	Description string      `json:"description"`
	Quantity    flexDecimal `json:"quantity"`
	UnitPrice   flexDecimal `json:"unitPrice"`
	Amount      flexDecimal `json:"amount"`
}

// flexDecimal reads a figure the model wrote as a string or a number.
type flexDecimal string

func (f *flexDecimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexDecimal(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*f = flexDecimal(n.String())
		return nil
	}
	*f = "" // null, or something that is not a figure
	return nil
}

// parseAnswer decodes a model reply, tolerating code fences and prose
// around the object.
func parseAnswer(text string) (answer, error) {
	body := jsonstream.StripFences(text)
	if start, end := strings.IndexByte(body, '{'), strings.LastIndexByte(body, '}'); start >= 0 && end > start {
		body = body[start : end+1]
	}
	var a answer
	err := json.Unmarshal([]byte(body), &a)
	return a, err
}

var kinds = map[string]string{
	"invoice": KindInvoice, "receipt": KindReceipt, "prepayment": KindPrepayment,
	"refund": KindRefund, "credit_note": KindCreditNote,
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// extraction checks and converts the answer. A field that does not hold up
// (an unknown kind, a malformed date or figure) is dropped and said so in
// the confidence notes, rather than failing the whole reading.
func (a answer) extraction() Extraction {
	var notes []string
	if n := strings.TrimSpace(a.ConfidenceNotes); n != "" {
		notes = append(notes, n)
	}
	e := Extraction{
		Kind:           kinds[strings.ToLower(strings.TrimSpace(a.Kind))],
		MerchantName:   strings.TrimSpace(a.MerchantName),
		DocumentNumber: strings.TrimSpace(a.DocumentNumber),
	}
	e.Currency = strings.ToUpper(strings.TrimSpace(a.Currency))
	if e.Currency != "" && !currencyCode.MatchString(e.Currency) {
		notes = append(notes, fmt.Sprintf("Currency %q is not an ISO 4217 code.", a.Currency))
		e.Currency = ""
	}
	date := func(name, v string) string {
		v = strings.TrimSpace(v)
		if v == "" {
			return ""
		}
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			notes = append(notes, fmt.Sprintf("The %s %q is not a date.", name, v))
			return ""
		}
		return v
	}
	e.IssueDate = date("issue date", a.IssueDate)
	e.ServicePeriodStart = date("service period start", a.ServicePeriodStart)
	e.ServicePeriodEnd = date("service period end", a.ServicePeriodEnd)
	figure := func(name string, v flexDecimal) *int64 {
		m, ok, err := micros(string(v))
		if err != nil {
			notes = append(notes, fmt.Sprintf("The %s %q is not a figure.", name, v))
		}
		if !ok {
			return nil
		}
		return &m
	}
	e.SubtotalMicros = figure("subtotal", a.Subtotal)
	e.TaxMicros = figure("tax", a.Tax)
	e.TotalMicros = figure("total", a.Total)
	for i, l := range a.LineItems {
		line := LineItem{Description: strings.TrimSpace(l.Description)}
		line.QuantityMicros = figure(fmt.Sprintf("line %d quantity", i+1), l.Quantity)
		line.UnitPriceMicros = figure(fmt.Sprintf("line %d unit price", i+1), l.UnitPrice)
		line.AmountMicros = figure(fmt.Sprintf("line %d amount", i+1), l.Amount)
		if line.Description == "" && line.AmountMicros == nil {
			continue
		}
		e.LineItems = append(e.LineItems, line)
	}
	e.ConfidenceNotes = strings.Join(notes, " ")
	return e
}

var figureNoise = strings.NewReplacer(" ", "", " ", "", "_", "", "$", "", "€", "", "£", "", "+", "", "-", "", "(", "", ")", "")

// micros converts a decimal figure to positive micros, exactly: "20.5" is
// 20500000. ok is false for an empty figure. A comma is read as a grouping
// separator, or as the decimal mark when it comes after the last dot
// ("1.234,56") or is the only separator with two or fewer digits after it
// ("12,50").
func micros(s string) (int64, bool, error) {
	s = figureNoise.Replace(strings.TrimSpace(s))
	if s == "" {
		return 0, false, nil
	}
	dot, comma := strings.LastIndexByte(s, '.'), strings.LastIndexByte(s, ',')
	switch {
	case dot >= 0 && comma > dot: // "1.234,56": the dots group, the comma is the mark
		s = strings.ReplaceAll(s, ".", "")
		s = strings.Replace(s, ",", ".", 1)
	case dot < 0 && comma >= 0 && strings.Count(s, ",") == 1 && len(s)-comma-1 <= 2:
		s = s[:comma] + "." + s[comma+1:]
	}
	s = strings.ReplaceAll(s, ",", "")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 6 {
		frac = frac[:6]
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false, err
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, false, err
	}
	if w > (1<<63-1)/1_000_000-1 {
		return 0, false, fmt.Errorf("figure %q out of range", s)
	}
	return w*1_000_000 + f, true, nil
}
