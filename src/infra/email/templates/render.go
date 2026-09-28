// Package templates renders Ogen's DB-stored email templates and seeds the
// built-in defaults on boot. Bodies are authored in Maizzle
// (build-time, outside this app) and stored as compiled HTML; this package only
// interpolates variables into them at send time.
package templates

import (
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Delimiters are deliberately NOT the Go default {{ }} — Maizzle/Tailwind output
// uses {{ }} too, so template variables use [[ ]] and any stray {{ }} in the
// compiled HTML passes through untouched. Every stored body + its data struct
// must follow this one convention.
const (
	LeftDelim  = "[["
	RightDelim = "]]"
)

// Rendered is the output of Render: the resolved subject plus HTML and text
// bodies, ready to drop into an email.Message.
type Rendered struct {
	Subject string
	HTML    string
	Text    string
}

// Render executes a stored template against data. Subject and text use
// text/template; HTML uses html/template for contextual auto-escaping (so a
// value interpolated into an href or body is escaped for that context). A parse
// or execute failure is returned to the caller, which treats it as terminal.
func Render(t *models.EmailTemplate, data any) (Rendered, error) {
	subject, err := renderText(t.Key+":subject", t.Subject, data)
	if err != nil {
		return Rendered{}, err
	}
	html, err := renderHTML(t.Key+":html", t.HTML, data)
	if err != nil {
		return Rendered{}, err
	}
	text, err := renderText(t.Key+":text", t.Text, data)
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{Subject: subject, HTML: html, Text: text}, nil
}

func renderHTML(name, src string, data any) (string, error) {
	tmpl, err := htmltemplate.New(name).Delims(LeftDelim, RightDelim).Parse(src)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func renderText(name, src string, data any) (string, error) {
	tmpl, err := texttemplate.New(name).Delims(LeftDelim, RightDelim).Parse(src)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}
