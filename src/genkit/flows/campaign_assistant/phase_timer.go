package campaign_assistant

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// phaseTimer times every stage of a turn so one "phase timings" line shows
// where a slow turn spends wall-clock: the pre-model DB phases, model TTFT,
// time to the first tool call, the whole generate call including inline tool
// execution, per-tool wall-time, and persist. The stream hooks run serially on
// the plugin's stream loop, so no locking is needed.
type phaseTimer struct {
	start time.Time
	last  time.Time
	laps  []any // "<phase>_ms", duration pairs in phase order

	genStart   time.Time
	genMs      int64
	firstChunk time.Time
	firstTool  time.Time
	toolStart  map[string]time.Time
	tools      []string // "<name>=<ms>ms" per completed tool call
	persistMs  int64
}

func newPhaseTimer() *phaseTimer {
	now := time.Now()
	return &phaseTimer{start: now, last: now, toolStart: map[string]time.Time{}}
}

// lap records the time since the previous lap as "<phase>_ms".
func (p *phaseTimer) lap(phase string) {
	now := time.Now()
	p.laps = append(p.laps, phase+"_ms", now.Sub(p.last).Milliseconds())
	p.last = now
}

func (p *phaseTimer) chunk() {
	if p.firstChunk.IsZero() {
		p.firstChunk = time.Now()
	}
}

func (p *phaseTimer) toolCalled(ref string) {
	now := time.Now()
	if p.firstTool.IsZero() {
		p.firstTool = now
	}
	p.toolStart[ref] = now
}

func (p *phaseTimer) toolReturned(name, ref string) {
	if s, ok := p.toolStart[ref]; ok {
		p.tools = append(p.tools, fmt.Sprintf("%s=%dms", name, time.Since(s).Milliseconds()))
	}
}

// sinceGen returns the milliseconds from the generate call's start to t, or
// -1 when t never happened.
func (p *phaseTimer) sinceGen(t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return t.Sub(p.genStart).Milliseconds()
}

func (p *phaseTimer) totalMs() int64 { return time.Since(p.start).Milliseconds() }

func (p *phaseTimer) log(ctx context.Context, campaignID, action string) {
	attrs := []any{logging.AttrComponent, logComponent, "campaign_id", campaignID}
	attrs = append(attrs, p.laps...)
	attrs = append(attrs,
		"ttft_ms", p.sinceGen(p.firstChunk),
		"route_ms", p.sinceGen(p.firstTool),
		"gen_ms", p.genMs,
		"tools", strings.Join(p.tools, ","),
		"persist_ms", p.persistMs,
		"total_ms", p.totalMs(),
		"action", action,
	)
	slog.InfoContext(ctx, "phase timings", attrs...)
}
