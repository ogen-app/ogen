// Package flowkit holds the plumbing shared by the Genkit flows: template
// rendering, stream callbacks, conversation history mapping, usage logging and
// streaming of JSON-array responses.
package flowkit

import (
	"bytes"
	"context"
	"text/template"

	"github.com/firebase/genkit/go/ai"
)

// Conversation roles stored on assistant message rows.
const (
	RoleUser  = "user"
	RoleModel = "model"
)

// RenderTemplate executes tmpl with data and returns the output.
func RenderTemplate(tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// History maps stored conversation rows to model messages, oldest first.
// turn extracts a row's role and content; rows with any other role are
// skipped.
func History[M any](rows []M, turn func(M) (role, content string)) []*ai.Message {
	out := make([]*ai.Message, 0, len(rows))
	for _, row := range rows {
		switch role, content := turn(row); role {
		case RoleUser:
			out = append(out, ai.NewUserTextMessage(content))
		case RoleModel:
			out = append(out, ai.NewModelTextMessage(content))
		}
	}
	return out
}

// StreamHandlers receives the parts of a streamed generation. Every field is
// optional.
type StreamHandlers struct {
	// OnChunk runs once per non-aggregated chunk, before its parts.
	OnChunk func()
	// OnText receives each text part.
	OnText func(text string)
	// OnToolCall runs once per complete (non-partial) tool request Ref.
	OnToolCall func(req *ai.ToolRequest)
	// OnToolResult runs once per tool response Ref.
	OnToolResult func(resp *ai.ToolResponse)
}

// StreamCallback adapts h to a genkit stream callback. Aggregated chunks are
// skipped because genkit replays the whole response in them, and tool parts
// are deduplicated by Ref because genkit may resend the same part in later
// chunks. The callback is not safe for concurrent use; genkit calls it
// serially.
func StreamCallback(h StreamHandlers) ai.ModelStreamCallback {
	d := &streamDispatcher{h: h, calls: map[string]bool{}, results: map[string]bool{}}
	return d.chunk
}

type streamDispatcher struct {
	h       StreamHandlers
	calls   map[string]bool
	results map[string]bool
}

func (d *streamDispatcher) chunk(_ context.Context, chunk *ai.ModelResponseChunk) error {
	if chunk == nil || chunk.Aggregated {
		return nil
	}
	if d.h.OnChunk != nil {
		d.h.OnChunk()
	}
	for _, part := range chunk.Content {
		switch {
		case part.IsText():
			if d.h.OnText != nil {
				d.h.OnText(part.Text)
			}
		case part.IsToolRequest():
			d.toolRequest(part.ToolRequest)
		case part.IsToolResponse():
			d.toolResponse(part.ToolResponse)
		}
	}
	return nil
}

func (d *streamDispatcher) toolRequest(tr *ai.ToolRequest) {
	if tr == nil || tr.Partial || d.calls[tr.Ref] {
		return
	}
	d.calls[tr.Ref] = true
	if d.h.OnToolCall != nil {
		d.h.OnToolCall(tr)
	}
}

func (d *streamDispatcher) toolResponse(tr *ai.ToolResponse) {
	if tr == nil || d.results[tr.Ref] {
		return
	}
	d.results[tr.Ref] = true
	if d.h.OnToolResult != nil {
		d.h.OnToolResult(tr)
	}
}
