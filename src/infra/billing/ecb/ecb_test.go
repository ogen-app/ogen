package ecb

import (
	"testing"
	"time"
)

// The shape of eurofxref-hist-90d.xml, trimmed to two days.
const sample = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
	<gesmes:subject>Reference rates</gesmes:subject>
	<gesmes:Sender><gesmes:name>European Central Bank</gesmes:name></gesmes:Sender>
	<Cube>
		<Cube time="2026-11-25">
			<Cube currency="USD" rate="1.0512"/>
			<Cube currency="JPY" rate="161.20"/>
		</Cube>
		<Cube time="2026-11-24">
			<Cube currency="USD" rate="1.0498"/>
		</Cube>
	</Cube>
</gesmes:Envelope>`

func TestParse(t *testing.T) {
	rates, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 3 {
		t.Fatalf("got %d rates, want 3", len(rates))
	}
	first := rates[0]
	if !first.Date.Equal(time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)) || first.Currency != "USD" || first.PerEUR.FloatString(4) != "1.0512" {
		t.Fatalf("first rate = %+v", first)
	}
	if _, err := Parse([]byte(`<Envelope><Cube/></Envelope>`)); err == nil {
		t.Fatal("a file with no rates parsed")
	}
	if _, err := Parse([]byte(`<Envelope><Cube><Cube time="2026-11-25"><Cube currency="USD" rate="x"/></Cube></Cube></Envelope>`)); err == nil {
		t.Fatal("a bad rate parsed")
	}
}
