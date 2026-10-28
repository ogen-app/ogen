package inbound

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

var pdfBytes = []byte("%PDF-1.4 receipt")

// gmailForward is a Stripe receipt forwarded inline from Gmail, its PDF
// invoice attached.
var gmailForward = crlf(`From: Ops <ops@getogen.com>
To: receipts@in.getogen.com
Subject: =?UTF-8?Q?Fwd:_Your_receipt_from_Railway_=E2=80=94_#2481?=
Date: Tue, 27 Oct 2026 09:15:00 +0000
Message-ID: <CAF-123@mail.gmail.com>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mixed"

--mixed
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset="UTF-8"
Content-Transfer-Encoding: quoted-printable

---------- Forwarded message ---------
From: Railway <invoice+statements@railway.app>
Date: Mon, Oct 26, 2026
Subject: Your receipt

Amount paid =E2=82=AC20.00
--alt
Content-Type: text/html; charset="UTF-8"
Content-Transfer-Encoding: base64

` + base64.StdEncoding.EncodeToString([]byte("<p>Amount paid <b>€20.00</b></p>")) + `
--alt--

--mixed
Content-Type: application/pdf; name="Invoice-2481.pdf"
Content-Disposition: attachment; filename="Invoice-2481.pdf"
Content-Transfer-Encoding: base64

` + base64.StdEncoding.EncodeToString(pdfBytes) + `
--mixed--
`)

func TestParseGmailForward(t *testing.T) {
	m, err := Parse(gmailForward)
	if err != nil {
		t.Fatal(err)
	}
	if m.From != "Ops <ops@getogen.com>" || len(m.To) != 1 || m.To[0] != "receipts@in.getogen.com" ||
		m.Subject != "Fwd: Your receipt from Railway — #2481" || m.MessageID != "CAF-123@mail.gmail.com" {
		t.Fatalf("headers = %+v", m)
	}
	if m.Date == nil || !m.Date.Equal(time.Date(2026, 10, 27, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("date = %v", m.Date)
	}
	if m.ForwardedFrom != "Railway <invoice+statements@railway.app>" {
		t.Fatalf("forwarded from = %q", m.ForwardedFrom)
	}
	if !strings.Contains(m.TextBody, "Amount paid €20.00") || m.HTMLBody != "<p>Amount paid <b>€20.00</b></p>" {
		t.Fatalf("bodies = %q / %q", m.TextBody, m.HTMLBody)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "Invoice-2481.pdf" || m.Attachments[0].ContentType != "application/pdf" ||
		string(m.Attachments[0].Content) != string(pdfBytes) {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
}

func TestParseForwardedAsAttachment(t *testing.T) {
	raw := crlf(`From: ops@getogen.com
To: receipts@in.getogen.com
Subject: Fwd: receipt
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain

--b
Content-Type: message/rfc822

From: Zernio <billing@zernio.com>
Subject: Receipt
Content-Type: text/html; charset=iso-8859-1

<p>Total 10=E9</p>
--b--
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.ForwardedFrom != "Zernio <billing@zernio.com>" || !strings.Contains(m.HTMLBody, "Total 10") || m.Date != nil {
		t.Fatalf("message = %+v", m)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not an email")); !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v", err)
	}
}

func TestForwardedFrom(t *testing.T) {
	cases := map[string]string{
		"Begin forwarded message:\n\nFrom: Cloudflare <noreply@notify.cloudflare.com>\nSubject: x": "Cloudflare <noreply@notify.cloudflare.com>",
		"hello\n-----Original Message-----\n> From: billing@resend.com\n":                          "billing@resend.com",
		"From: not a forward": "",
	}
	for body, want := range cases {
		if got := ForwardedFrom(body); got != want {
			t.Errorf("ForwardedFrom(%q) = %q, want %q", body, got, want)
		}
	}
}
