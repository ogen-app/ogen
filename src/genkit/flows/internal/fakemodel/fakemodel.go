// Package fakemodel is a scripted genkit model for testing assistant runners
// without a provider key. Each model call consumes the next scripted turn:
// a turn either requests tools (genkit runs them and calls the model again)
// or answers with final text. Every request is captured, so a test can assert
// what the runner sent and what the tools returned.
package fakemodel

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// those tools, after any Text; a turn without Calls ends the loop with Text.
// Text is streamed in Chunks when they are set, otherwise in one piece.
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
	// g is the instance the model is registered on, where Route registers
	// the unrouted model too.
	g *genkit.Genkit

	mu       sync.Mutex
	script   []Turn
	next     int
	requests []*ai.ModelRequest
}

// Define registers a scripted model named "fake/<name>" on g.
func Define(g *genkit.Genkit, name string, script ...Turn) *Model {
	m := &Model{name: name, g: g, script: script}
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
	// A turn with Calls streams Text only when it is set; a final turn always
	// answers, even with nothing.
	chunks := t.Chunks
	if len(chunks) == 0 && (t.Text != "" || len(t.Calls) == 0) {
		chunks = []string{t.Text}
	}
	var parts []*ai.Part
	if len(chunks) > 0 {
		if cb != nil {
			for _, c := range chunks {
				if err := cb(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: []*ai.Part{ai.NewTextPart(c)}}); err != nil {
					return nil, err
				}
			}
		}
		parts = append(parts, ai.NewTextPart(strings.Join(chunks, "")))
	}
	var calls []*ai.Part
	for i, c := range t.Calls {
		calls = append(calls, ai.NewToolRequestPart(&ai.ToolRequest{
			Name: c.Name, Input: c.Input, Ref: fmt.Sprintf("call-%d-%d", n, i),
		}))
	}
	if len(calls) > 0 && cb != nil {
		if err := cb(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: calls}); err != nil {
			return nil, err
		}
	}
	parts = append(parts, calls...)
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

// unrouted is the model every slot Route was not given resolves to: a call
// to it fails, naming the cause, rather than reaching a model that was
// never registered.
const unrouted = "unrouted"

// ErrUnrouted is a call to a slot no test routed a script to.
var ErrUnrouted = errors.New("fakemodel: no script is routed to this slot; pass it to Route")

// Route points each slot at m in the global model-config resolver for the
// lifetime of ctx; every other slot resolves to a model that fails with
// ErrUnrouted. When ctx ends, the resolver Route replaced is restored. The
// resolver is process-global, so tests that call Route must not run in
// parallel with other tests that resolve models.
func Route(ctx context.Context, m *Model, slots ...Slot) {
	routes := make(map[Slot]*Model, len(slots))
	for _, s := range slots {
		routes[s] = m
	}
	RouteEach(ctx, routes)
}

// RouteEach points each slot at its own model, as Route does, for a flow
// whose slots run different scripts — an orchestrator and its writer.
func RouteEach(ctx context.Context, routes map[Slot]*Model) {
	rows := make([]models.FlowModelConfig, 0, len(routes))
	for s, m := range routes {
		defineUnrouted(m.g)
		rows = append(rows, models.FlowModelConfig{FlowKey: s.Flow, SlotKey: s.Slot, ModelID: m.name})
	}
	in := &install{prev: modelconfig.Current()}
	installsMu.Lock()
	installs = append(installs, in)
	installsMu.Unlock()
	modelconfig.Init(ctx, &memSource{rows: rows}, unroutedDefaults,
		func(string) (string, bool) { return Vendor, true },
		func(context.Context) (string, bool) { return "", false })
	context.AfterFunc(ctx, in.remove)
}

// install is one Route's resolver, and the one it replaced.
type install struct{ prev modelconfig.State }

// installs are the resolvers Route put in place, oldest first; the last is
// the one serving.
var (
	installsMu sync.Mutex
	installs   []*install
)

// remove ends an install, in whatever order the contexts end: the serving
// one gives way to what it replaced; an older one hands what it replaced to
// the install above it, so the resolver from before the first Route is the
// one left in the end.
func (in *install) remove() {
	installsMu.Lock()
	defer installsMu.Unlock()
	i := slices.Index(installs, in)
	if i < 0 {
		return
	}
	if i == len(installs)-1 {
		in.prev.Restore()
	} else {
		installs[i+1].prev = in.prev
	}
	installs = slices.Delete(installs, i, i+1)
}

// unroutedDefaults seed every slot Route was not given with the unrouted
// model.
var unroutedDefaults = modelconfig.Defaults{
	Generation: unrouted, Quality: unrouted, Planning: unrouted, Embed: unrouted,
	VisionClassify: unrouted, VisionExtract: unrouted, VisionEscalate: unrouted,
	Transcribe: unrouted, Receipts: unrouted,
}

// defineUnrouted registers the unrouted model on g once.
func defineUnrouted(g *genkit.Genkit) {
	if genkit.LookupModel(g, Vendor+"/"+unrouted) != nil {
		return
	}
	genkit.DefineModel(g, Vendor+"/"+unrouted,
		&ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true, SystemRole: true, Tools: true}},
		func(context.Context, *ai.ModelRequest, ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			return nil, ErrUnrouted
		})
}

// memSource is an in-memory modelconfig.Source: the scripted rows, plus the
// unrouted defaults reconcile writes for every other slot.
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
