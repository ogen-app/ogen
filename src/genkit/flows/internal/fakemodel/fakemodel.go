// Package fakemodel is a scripted genkit model for testing assistant runners
// without a provider key. Each model call consumes the next scripted turn:
// a turn either requests tools (genkit runs them and calls the model again)
// or answers with final text. Every request is captured, so a test can assert
// what the runner sent and what the tools returned.
package fakemodel

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
)

// Vendor is the vendor slug every fake model is registered under, so a ref
// reads "fake/<name>".
const Vendor = "fake"

// Call is one tool request the model emits.
type Call struct {
	Name  string
	Input any
}

// Turn is one scripted model response. A turn with Calls asks genkit to run
// those tools; a turn without Calls ends the loop with Text. Text is streamed
// in Chunks when they are set, otherwise in one piece.
type Turn struct {
	Calls  []Call
	Text   string
	Chunks []string
}

// Tools returns a turn that requests the given tools.
func Tools(calls ...Call) Turn { return Turn{Calls: calls} }

// Answer returns a final-text turn.
func Answer(text string) Turn { return Turn{Text: text} }

// Model replays a script and records every request it receives.
type Model struct {
	name string

	mu       sync.Mutex
	script   []Turn
	next     int
	requests []*ai.ModelRequest
}

// Define registers a scripted model named "fake/<name>" on g.
func Define(g *genkit.Genkit, name string, script ...Turn) *Model {
	m := &Model{name: name, script: script}
	genkit.DefineModel(g, Vendor+"/"+name,
		&ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true, SystemRole: true, Tools: true}},
		m.generate)
	return m
}

// Ref is the genkit model reference to pass to ai.WithModelName.
func (m *Model) Ref() string { return Vendor + "/" + m.name }

// Requests returns every request the model received, in order.
func (m *Model) Requests() []*ai.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*ai.ModelRequest(nil), m.requests...)
}

// Remaining reports how many scripted turns were not consumed.
func (m *Model) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.script) - m.next
}

// ToolResponses returns the tool results genkit fed back to the model, across
// all requests, in order.
func (m *Model) ToolResponses() []*ai.ToolResponse {
	var out []*ai.ToolResponse
	seen := make(map[string]bool)
	for _, req := range m.Requests() {
		for _, msg := range req.Messages {
			for _, p := range msg.Content {
				if !p.IsToolResponse() {
					continue
				}
				key := p.ToolResponse.Name + "\x00" + p.ToolResponse.Ref
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, p.ToolResponse)
			}
		}
	}
	return out
}

func (m *Model) take(req *ai.ModelRequest) (Turn, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	if m.next >= len(m.script) {
		return Turn{}, 0, fmt.Errorf("fakemodel %s: script exhausted after %d turns", m.name, len(m.script))
	}
	t := m.script[m.next]
	m.next++
	return t, m.next, nil
}

func (m *Model) generate(ctx context.Context, req *ai.ModelRequest, cb ai.ModelStreamCallback) (*ai.ModelResponse, error) {
	t, n, err := m.take(req)
	if err != nil {
		return nil, err
	}
	var parts []*ai.Part
	for i, c := range t.Calls {
		parts = append(parts, ai.NewToolRequestPart(&ai.ToolRequest{
			Name: c.Name, Input: c.Input, Ref: fmt.Sprintf("call-%d-%d", n, i),
		}))
	}
	if len(t.Calls) == 0 {
		chunks := t.Chunks
		if len(chunks) == 0 {
			chunks = []string{t.Text}
		}
		if cb != nil {
			for _, c := range chunks {
				if err := cb(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: []*ai.Part{ai.NewTextPart(c)}}); err != nil {
					return nil, err
				}
			}
		}
		parts = append(parts, ai.NewTextPart(strings.Join(chunks, "")))
	} else if cb != nil {
		if err := cb(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: parts}); err != nil {
			return nil, err
		}
	}
	return &ai.ModelResponse{
		Message:      ai.NewModelMessage(parts...),
		FinishReason: ai.FinishReasonStop,
		Request:      req,
	}, nil
}

// Slot names one flow slot in the model-config catalog.
type Slot struct {
	Flow string
	Slot string
}

// Route points each slot at m in the global model-config resolver for the
// lifetime of ctx. The resolver is process-global, so tests that call Route
// must not run in parallel with other tests that resolve models.
func Route(ctx context.Context, m *Model, slots ...Slot) {
	rows := make([]models.FlowModelConfig, 0, len(slots))
	for _, s := range slots {
		rows = append(rows, models.FlowModelConfig{FlowKey: s.Flow, SlotKey: s.Slot, ModelID: m.name})
	}
	modelconfig.Init(ctx, &memSource{rows: rows}, modelconfig.Defaults{},
		func(string) (string, bool) { return Vendor, true },
		func(context.Context) (string, bool) { return "", false })
}

// memSource is an in-memory modelconfig.Source: the scripted rows, plus the
// seeded defaults reconcile writes for every other slot.
type memSource struct {
	mu   sync.Mutex
	rows []models.FlowModelConfig
}

func (s *memSource) List(context.Context) ([]models.FlowModelConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.FlowModelConfig(nil), s.rows...), nil
}

func (s *memSource) Upsert(_ context.Context, cfg *models.FlowModelConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, *cfg)
	return nil
}
