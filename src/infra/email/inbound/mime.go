// Package inbound parses a received email (RFC 5322 MIME) into its headers,
// bodies and attachments. It is tolerant: a part that does not decode is
// kept raw or skipped rather than failing the message, because a receipt
// with one odd part is still a receipt. Only a message whose header block
// does not parse is an error.
package inbound

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
)

// MaxRawBytes is the largest email accepted: the common mail-provider cap.
const MaxRawBytes = 25 << 20

// maxDepth bounds multipart and message/rfc822 nesting.
const maxDepth = 10

// ErrMalformed is a message whose headers do not parse.
var ErrMalformed = errors.New("inbound: malformed message")

// Attachment is one decoded attachment.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

// Message is a parsed email.
type Message struct {
	From    string
	To      []string
	Subject string
	// Date is nil when the header is missing or does not parse.
	Date *time.Time
	// MessageID is without angle brackets.
	MessageID string
	// ForwardedFrom is the From of the email this one forwards: from a
	// forwarded message/rfc822 part, else from the forwarded block a mail
	// client writes into the body. "" when neither is found.
	ForwardedFrom string
	TextBody      string
	HTMLBody      string
	Attachments   []Attachment
}

var decoder = mime.WordDecoder{CharsetReader: charset.NewReaderLabel}

// Parse reads raw as a MIME message.
func Parse(raw []byte) (*Message, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	m := &Message{
		From:      decodeHeader(msg.Header.Get("From")),
		Subject:   decodeHeader(msg.Header.Get("Subject")),
		MessageID: strings.Trim(strings.TrimSpace(msg.Header.Get("Message-Id")), "<>"),
	}
	if list, err := addressList(msg.Header.Get("To")); err == nil {
		m.To = list
	} else if v := strings.TrimSpace(msg.Header.Get("To")); v != "" {
		m.To = []string{decodeHeader(v)}
	}
	if d, err := msg.Header.Date(); err == nil {
		m.Date = &d
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	p := &parser{}
	p.part(textproto(msg.Header), body, 0)
	m.TextBody, m.HTMLBody, m.Attachments = p.text, p.html, p.attachments
	m.ForwardedFrom = p.forwardedFrom
	if m.ForwardedFrom == "" {
		m.ForwardedFrom = ForwardedFrom(nonBlank(m.TextBody, htmlText(m.HTMLBody)))
	}
	return m, nil
}

// nonBlank is a unless it is only whitespace, else b.
func nonBlank(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func addressList(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	addrs, err := (&mail.AddressParser{WordDecoder: &decoder}).ParseList(v)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Name == "" {
			out = append(out, a.Address)
			continue
		}
		out = append(out, a.String())
	}
	return out, nil
}

func decodeHeader(v string) string {
	if d, err := decoder.DecodeHeader(v); err == nil {
		return strings.TrimSpace(d)
	}
	return strings.TrimSpace(v)
}

// header is the part of a MIME header a part needs.
type header interface {
	Get(key string) string
}

type textproto mail.Header

func (h textproto) Get(key string) string { return mail.Header(h).Get(key) }

// parser walks the part tree, keeping the first text and HTML body it meets
// outside attachments, and every attachment.
type parser struct {
	text, html    string
	attachments   []Attachment
	forwardedFrom string
}

func (p *parser) part(h header, body []byte, depth int) {
	if depth > maxDepth {
		return
	}
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		mediaType, params = "text/plain", map[string]string{}
	}
	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := decodeHeader(nonBlank(dparams["filename"], params["name"]))
	data := decodeTransfer(h.Get("Content-Transfer-Encoding"), body)

	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		p.multipart(data, params["boundary"], depth)
	case mediaType == "message/rfc822":
		p.forwarded(data, depth)
	case disposition == "attachment" || (filename != "" && !strings.HasPrefix(mediaType, "text/")):
		p.attachments = append(p.attachments, Attachment{Filename: filename, ContentType: mediaType, Content: data})
	case mediaType == "text/html" && p.html == "":
		p.html = decodeCharset(params["charset"], data)
	case mediaType == "text/plain" && p.text == "":
		p.text = decodeCharset(params["charset"], data)
	case strings.HasPrefix(mediaType, "text/"):
		// A second body of a kind already kept (an alternative, a footer).
	default:
		// An inline image or other unnamed part.
		p.attachments = append(p.attachments, Attachment{Filename: filename, ContentType: mediaType, Content: data})
	}
}

func (p *parser) multipart(data []byte, boundary string, depth int) {
	if boundary == "" {
		return
	}
	r := multipart.NewReader(bytes.NewReader(data), boundary)
	for {
		part, err := r.NextRawPart()
		if err != nil {
			return
		}
		body, err := io.ReadAll(part)
		if err != nil {
			return
		}
		p.part(part.Header, body, depth+1)
	}
}

// forwarded reads a message forwarded as an attachment: its From is the
// forwarded sender, and its bodies stand in for the outer ones the
// forwarder left empty.
func (p *parser) forwarded(data []byte, depth int) {
	inner, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		p.attachments = append(p.attachments, Attachment{Filename: "forwarded.eml", ContentType: "message/rfc822", Content: data})
		return
	}
	if p.forwardedFrom == "" {
		p.forwardedFrom = decodeHeader(inner.Header.Get("From"))
	}
	body, err := io.ReadAll(inner.Body)
	if err != nil {
		return
	}
	sub := &parser{}
	sub.part(textproto(inner.Header), body, depth+1)
	if strings.TrimSpace(p.text) == "" {
		p.text = sub.text
	}
	if strings.TrimSpace(p.html) == "" {
		p.html = sub.html
	}
	p.attachments = append(p.attachments, sub.attachments...)
}

func decodeTransfer(encoding string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, body)
		out := make([]byte, base64.StdEncoding.DecodedLen(len(clean)))
		n, err := base64.StdEncoding.Decode(out, clean)
		if err != nil {
			// Unpadded or truncated: keep what decoded.
			if n2, err2 := base64.RawStdEncoding.Decode(out, bytes.TrimRight(clean, "=")); err2 == nil {
				return out[:n2]
			}
		}
		return out[:n]
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil && len(out) == 0 {
			return body
		}
		return out
	default:
		return body
	}
}

func decodeCharset(label string, data []byte) string {
	if label == "" || strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "us-ascii") {
		return string(data)
	}
	r, err := charset.NewReaderLabel(label, bytes.NewReader(data))
	if err != nil {
		return string(data)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return string(data)
	}
	return string(out)
}

var (
	forwardMarker = regexp.MustCompile(`(?i)(forwarded message|begin forwarded message|original message)`)
	fromLine      = regexp.MustCompile(`(?im)^[>\s*]*from:\s*(.+?)\s*$`)
)

// ForwardedFrom finds the From line of the forwarded block a mail client
// writes into the body (Gmail, Apple Mail, Outlook), or "".
func ForwardedFrom(body string) string {
	loc := forwardMarker.FindStringIndex(body)
	if loc == nil {
		return ""
	}
	m := fromLine.FindStringSubmatch(body[loc[1]:])
	if m == nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(m[1]), "*")
}

var (
	htmlDrop  = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)>`)
	htmlBreak = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/li|/h[1-6])\b[^>]*>`)
	htmlTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRuns = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
)

// htmlText is a rough plain-text rendering of an HTML body, for finding the
// forwarded block.
func htmlText(html string) string {
	s := htmlDrop.ReplaceAllString(html, "")
	s = htmlBreak.ReplaceAllString(s, "\n")
	s = htmlTag.ReplaceAllString(s, "")
	s = strings.NewReplacer("&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`, "&#39;", "'").Replace(s)
	return blankRuns.ReplaceAllString(s, "\n")
}
