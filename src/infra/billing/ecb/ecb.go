// Package ecb reads the European Central Bank's euro foreign exchange
// reference rates: one rate per currency per business day, published around
// 16:00 CET, free and keyless.
package ecb

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// HistoryURL is the last 90 business days of rates. Reading it every time
// fills any gap the daily file would leave after a missed run.
const HistoryURL = "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-hist-90d.xml"

// envelope is the eurofxref document: Cube/Cube[@time]/Cube[@currency,@rate].
type envelope struct {
	Days []struct {
		Time  string `xml:"time,attr"`
		Rates []struct {
			Currency string `xml:"currency,attr"`
			Rate     string `xml:"rate,attr"`
		} `xml:"Cube"`
	} `xml:"Cube>Cube"`
}

// Parse reads a eurofxref file (daily or history) into rates.
func Parse(raw []byte) ([]billing.FXRate, error) {
	var env envelope
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("ecb: decode rates: %w", err)
	}
	var out []billing.FXRate
	for _, d := range env.Days {
		date, err := time.Parse(time.DateOnly, strings.TrimSpace(d.Time))
		if err != nil {
			return nil, fmt.Errorf("ecb: bad day %q: %w", d.Time, err)
		}
		for _, r := range d.Rates {
			per, err := billing.ParseRate(r.Rate)
			if err != nil {
				return nil, err
			}
			out = append(out, billing.FXRate{Date: date, Currency: strings.ToUpper(strings.TrimSpace(r.Currency)), PerEUR: per})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ecb: no rates in the file")
	}
	return out, nil
}
