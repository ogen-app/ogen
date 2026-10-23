// Package templates renders Ogen's DB-stored email templates and seeds the
// built-in defaults on boot. Bodies are authored in Maizzle
// (build-time, outside this app) and stored as compiled HTML; this package only
// interpolates variables into them at send time.
package templates

import (
	"crypto/sha256"
	"errors"
	htmltemplate "html/template"
	"strings"
	"sync"
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

// funcs are the helpers every template may call. dict builds a map from
// key/value pairs so a body can pass several values to a sub-template.
var funcs = map[string]any{
	"dict": func(kv ...any) (map[string]any, error) {
		if len(kv)%2 != 0 {
			return nil, errors.New("dict: odd number of arguments")
		}
		m := make(map[string]any, len(kv)/2)
		for i := 0; i < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				return nil, errors.New("dict: keys must be strings")
			}
			m[k] = kv[i+1]
		}
		return m, nil
	},
}

func renderHTML(name, src string, data any) (string, error) {
	tmpl, err := parsed(htmlTemplates, name, src, func() (*htmltemplate.Template, error) {
		return htmltemplate.New(name).Delims(LeftDelim, RightDelim).Funcs(funcs).Parse(src)
	})
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
	tmpl, err := parsed(textTemplates, name, src, func() (*texttemplate.Template, error) {
		return texttemplate.New(name).Delims(LeftDelim, RightDelim).Funcs(funcs).Parse(src)
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Parsed templates are cached by name and source, so a send reuses the parse
// of the same stored body instead of re-parsing the full compiled HTML. An
// edited body has new source and parses fresh. Parsed templates are safe to
// execute concurrently.
var (
	htmlTemplates = newTemplateCache[*htmltemplate.Template]()
	textTemplates = newTemplateCache[*texttemplate.Template]()
)

// templateCacheMax bounds each cache; past it the cache starts over, which
// only costs a re-parse.
const templateCacheMax = 256

type templateCache[T any] struct {
	mu sync.Mutex
	m  map[[sha256.Size]byte]T
}

func newTemplateCache[T any]() *templateCache[T] {
	return &templateCache[T]{m: map[[sha256.Size]byte]T{}}
}

func parsed[T any](c *templateCache[T], name, src string, parse func() (T, error)) (T, error) {
	key := sha256.Sum256([]byte(name + "\x00" + src))
	c.mu.Lock()
	t, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return t, nil
	}
	t, err := parse()
	if err != nil {
		return t, err
	}
	c.mu.Lock()
	if len(c.m) >= templateCacheMax {
		clear(c.m)
	}
	c.m[key] = t
	c.mu.Unlock()
	return t, nil
}
