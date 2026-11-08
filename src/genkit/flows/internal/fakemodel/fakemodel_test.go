package fakemodel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
)

type addIn struct {
	A int `json:"a"`
	B int `json:"b"`
}

type addOut struct {
	Sum int `json:"sum"`
}

func defineAdd(g *genkit.Genkit) ai.Tool {
	return genkit.DefineTool(g, "add", "adds two numbers",
		func(_ *ai.ToolContext, in addIn) (addOut, error) { return addOut{Sum: in.A + in.B}, nil })
}

func TestModel_RunsToolLoopThenAnswers(t *testing.T) {
	g := genkit.Init(t.Context())
	add := defineAdd(g)
	m := Define(g, "loop",
		Tools(Call{Name: "add", Input: map[string]any{"a": 1, "b": 2}}),
		Answer(`{"explanation":"3"}`))

	resp, err := genkit.Generate(t.Context(), g,
		ai.WithModelName(m.Ref()), ai.WithPrompt("add 1 and 2"), ai.WithTools(add), ai.WithMaxTurns(2))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Text(); got != `{"explanation":"3"}` {
		t.Fatalf("text = %q", got)
	}
	if n := len(m.Requests()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	if m.Remaining() != 0 {
		t.Fatalf("remaining = %d, want 0", m.Remaining())
	}
	trs := m.ToolResponses()
	if len(trs) != 1 || trs[0].Name != "add" {
		t.Fatalf("tool responses = %+v", trs)
	}
	if out, ok := trs[0].Output.(map[string]any); !ok || out["sum"] != float64(3) {
		t.Fatalf("tool output = %#v", trs[0].Output)
	}
}

func TestModel_StreamsChunks(t *testing.T) {
	g := genkit.Init(t.Context())
	m := Define(g, "stream", Turn{Chunks: []string{`{"expl`, `anation":"hi"}`}})

	var got []string
	resp, err := genkit.Generate(t.Context(), g, ai.WithModelName(m.Ref()), ai.WithPrompt("hi"),
		ai.WithStreaming(func(_ context.Context, c *ai.ModelResponseChunk) error {
			got = append(got, c.Text())
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != `{"expl|anation":"hi"}` {
		t.Fatalf("chunks = %q", got)
	}
	if resp.Text() != `{"explanation":"hi"}` {
		t.Fatalf("text = %q", resp.Text())
	}
}

func TestModel_ExhaustedScriptFails(t *testing.T) {
	g := genkit.Init(t.Context())
	add := defineAdd(g)
	m := Define(g, "short", Tools(Call{Name: "add", Input: map[string]any{"a": 1, "b": 1}}))

	_, err := genkit.Generate(t.Context(), g,
		ai.WithModelName(m.Ref()), ai.WithPrompt("go"), ai.WithTools(add), ai.WithMaxTurns(3))
	if err == nil || !strings.Contains(err.Error(), "script exhausted") {
		t.Fatalf("err = %v, want script exhausted", err)
	}
}

// A slot nobody routed fails with a reason, and the resolver Route
// replaced is back once its context ends.
func TestRoute_UnroutedSlotsFailAndTheResolverIsRestored(t *testing.T) {
	g := genkit.Init(t.Context())
	// The resolver in place before: the test's own routing of the writer.
	Route(t.Context(), Define(g, "outer", Answer("ok")), Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotWriter})
	before := modelconfig.Resolve(t.Context(), modelconfig.FlowAssistant, modelconfig.SlotWriter)

	ctx, cancel := context.WithCancel(t.Context())
	m := Define(g, "planner", Answer("ok"))
	Route(ctx, m, Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})
	// Routing twice on one instance registers the unrouted model once.
	Route(ctx, m, Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})

	writer := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotWriter)
	if writer.Ref != Vendor+"/"+unrouted {
		t.Fatalf("unrouted writer = %+v", writer)
	}
	if _, err := genkit.Generate(ctx, g, ai.WithModelName(writer.Ref), ai.WithPrompt("go")); !errors.Is(err, ErrUnrouted) {
		t.Fatalf("err = %v, want ErrUnrouted", err)
	}

	cancel()
	deadline := time.Now().Add(time.Second)
	for modelconfig.Resolve(t.Context(), modelconfig.FlowAssistant, modelconfig.SlotWriter) != before {
		if time.Now().After(deadline) {
			t.Fatalf("after the context ended the writer resolves to %+v, want %+v",
				modelconfig.Resolve(t.Context(), modelconfig.FlowAssistant, modelconfig.SlotWriter), before)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRoute_ResolvesSlotToModel(t *testing.T) {
	g := genkit.Init(t.Context())
	m := Define(g, "routed", Answer("ok"))
	Route(t.Context(), m, Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})

	mc := modelconfig.Resolve(t.Context(), modelconfig.FlowAssistant, modelconfig.SlotOrchestrator)
	if mc.Ref != m.Ref() || mc.Vendor != Vendor {
		t.Fatalf("resolved = %+v, want ref %q", mc, m.Ref())
	}
	resp, err := genkit.Generate(t.Context(), g, ai.WithModelName(mc.Ref), ai.WithPrompt("go"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "ok" {
		t.Fatalf("text = %q", resp.Text())
	}
}
