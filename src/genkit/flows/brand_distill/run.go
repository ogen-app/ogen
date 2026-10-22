package brand_distill

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const logComponent = "genkit.brand_distill"

const (
	defaultPartTokens       = 30_000
	defaultMaxParallelParts = 4
	extractMaxTokens        = int64(8192)
	composeMaxTokens        = int64(16384)
	// maxCandidatesPerPart and maxCandidates bound the evidence one run keeps.
	maxCandidatesPerPart = 40
	maxCandidates        = 400
	maxQuoteRunes        = 600
	maxNoteRunes         = 200
)

// modelCall is one generation the runner asks for.
type modelCall struct {
	Model     modelconfig.Resolved
	System    string
	Prompt    string
	MaxTokens int64
}

// generator runs one model call and returns its text. Production wraps
// genkit.Generate with metering; tests stub it.
type generator func(ctx context.Context, call modelCall) (string, error)

type prompts struct {
	extractSystem, extractPart    *template.Template
	composeSystem, composeRequest *template.Template
}

type runner struct {
	cfg     FlowConfig
	tpl     prompts
	gen     generator
	resolve func(ctx context.Context, flowKey, slotKey string) modelconfig.Resolved
}

type partData struct {
	Part, Parts int
	Segments    []Segment
}

// extract reads the segments in parts, in parallel, and keeps the passages
// that are in their segment word for word.
func (r *runner) extract(ctx context.Context, req ExtractRequest) (*ExtractResponse, error) {
	start := time.Now()
	if err := r.cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	segs := withText(req.Segments)
	if len(segs) == 0 {
		return nil, &ValidationError{Msg: "the sources have no text to read"}
	}
	model := r.resolve(ctx, modelconfig.FlowBrandDistill, modelconfig.SlotExtract)
	system, err := flowkit.RenderTemplate(r.tpl.extractSystem, nil)
	if err != nil {
		return nil, fmt.Errorf("render extract system prompt: %w", err)
	}
	parts := groups(segs, cmp.Or(r.cfg.PartTokens, defaultPartTokens))
	found := make([][]models.DistillCandidate, len(parts))
	errs := make([]error, len(parts))
	sem := make(chan struct{}, cmp.Or(r.cfg.MaxParallelParts, defaultMaxParallelParts))
	var wg sync.WaitGroup
	for i, part := range parts {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			found[i], errs[i] = r.extractPart(ctx, system, partData{Part: i + 1, Parts: len(parts), Segments: part}, model)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	resp := &ExtractResponse{Parts: len(parts), ModelID: model.Model}
	resp.Candidates, resp.Dropped = keepVerbatim(slices.Concat(found...), segs)
	slog.InfoContext(ctx, "read the sources", logging.AttrComponent, logComponent,
		"segments", len(segs), "parts", len(parts), "candidates", len(resp.Candidates),
		"dropped", resp.Dropped, "duration_ms", time.Since(start).Milliseconds())
	return resp, nil
}

func (r *runner) extractPart(ctx context.Context, system string, data partData, model modelconfig.Resolved) ([]models.DistillCandidate, error) {
	prompt, err := flowkit.RenderTemplate(r.tpl.extractPart, data)
	if err != nil {
		return nil, fmt.Errorf("render part %d: %w", data.Part, err)
	}
	var out struct {
		Candidates []models.DistillCandidate `json:"candidates"`
	}
	if err := r.generateJSON(ctx, modelCall{Model: model, System: system, Prompt: prompt, MaxTokens: extractMaxTokens}, &out); err != nil {
		return nil, err
	}
	return out.Candidates[:min(len(out.Candidates), maxCandidatesPerPart)], nil
}

// keepVerbatim keeps the candidates of a known kind whose quote is in their
// segment, trimmed and deduplicated, up to maxCandidates. It returns how many
// it dropped.
func keepVerbatim(in []models.DistillCandidate, segs []Segment) ([]models.DistillCandidate, int) {
	text := make(map[string]string, len(segs))
	for _, s := range segs {
		text[s.Ref] = s.Text
	}
	seen := map[string]bool{}
	out := make([]models.DistillCandidate, 0, len(in))
	for _, c := range in {
		c.Kind, c.Ref = strings.TrimSpace(c.Kind), strings.TrimSpace(c.Ref)
		c.Quote = clampRunes(strings.TrimSpace(c.Quote), maxQuoteRunes)
		c.Note = clampRunes(strings.TrimSpace(c.Note), maxNoteRunes)
		body, ok := text[c.Ref]
		key := c.Kind + "\x00" + Normalize(c.Quote)
		if !ok || !slices.Contains(CandidateKinds, c.Kind) || !Verbatim(body, c.Quote) || seen[key] || len(out) == maxCandidates {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out, len(in) - len(out)
}

// passageGroup is one kind of passage as the compose prompt lists it.
type passageGroup struct {
	Title    string
	Passages []passage
}

type passage struct {
	Ref, Label, Quote string
}

type composeData struct {
	Today   string
	Digest  Digest
	Empty   bool
	Groups  []passageGroup
	Answers []Answer
}

var groupTitles = map[string]string{
	KindVoice:       "How the brand writes",
	KindAudience:    "Who it writes for",
	KindFact:        "Facts",
	KindClaim:       "Claims",
	KindRestriction: "What it does not do or say",
	KindDisclaimer:  "Small print",
	KindTerm:        "Its words",
}

// compose writes the proposal from the passages and answers.
func (r *runner) compose(ctx context.Context, req ComposeRequest) (*ComposeResponse, error) {
	start := time.Now()
	if err := r.cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	if len(req.Candidates) == 0 && len(req.Answers) == 0 {
		return nil, &ValidationError{Msg: "nothing to compose from"}
	}
	model := r.resolve(ctx, modelconfig.FlowBrandDistill, modelconfig.SlotCompose)
	system, err := flowkit.RenderTemplate(r.tpl.composeSystem, nil)
	if err != nil {
		return nil, fmt.Errorf("render compose system prompt: %w", err)
	}
	prompt, err := flowkit.RenderTemplate(r.tpl.composeRequest, composeInput(req))
	if err != nil {
		return nil, fmt.Errorf("render compose request: %w", err)
	}
	var draft Draft
	if err := r.generateJSON(ctx, modelCall{Model: model, System: system, Prompt: prompt, MaxTokens: composeMaxTokens}, &draft); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "composed a brand proposal", logging.AttrComponent, logComponent,
		"candidates", len(req.Candidates), "answers", len(req.Answers),
		"voices", len(draft.Voices), "audiences", len(draft.Audiences), "facts", len(draft.Facts),
		"questions", len(draft.Questions), "duration_ms", time.Since(start).Milliseconds())
	return &ComposeResponse{Draft: draft, ModelID: model.Model}, nil
}

// composeInput groups the passages by kind and makes every user-written text
// safe to place between the prompt's tags.
func composeInput(req ComposeRequest) composeData {
	d := req.Digest
	data := composeData{
		Today:  req.Today,
		Digest: d,
		Empty: len(d.Voices) == 0 && len(d.Audiences) == 0 && len(d.MayClaim) == 0 && len(d.NeverClaim) == 0 &&
			len(d.BannedWords) == 0 && d.Disclaimer == "" && len(d.Facts) == 0,
	}
	for _, kind := range CandidateKinds {
		g := passageGroup{Title: groupTitles[kind]}
		for _, c := range req.Candidates {
			if c.Kind == kind {
				g.Passages = append(g.Passages, passage{Ref: c.Ref, Label: untag(req.Labels[c.Ref]), Quote: untag(c.Quote)})
			}
		}
		if len(g.Passages) > 0 {
			data.Groups = append(data.Groups, g)
		}
	}
	for _, a := range req.Answers {
		data.Answers = append(data.Answers, Answer{QuestionID: a.QuestionID, Question: untag(a.Question), Answer: untag(a.Answer)})
	}
	return data
}

// tags keeps material from closing or opening the prompt's own tags.
var tags = strings.NewReplacer("<", "‹", ">", "›")

func untag(s string) string { return tags.Replace(s) }

// generateJSON runs call and decodes its reply into v. A reply that is not
// valid JSON gets one repair turn; a second failure is an invalid-output
// AIError, since the same prompt would most likely fail again.
func (r *runner) generateJSON(ctx context.Context, call modelCall, v any) error {
	text, err := r.gen(ctx, call)
	if err != nil {
		return err
	}
	parseErr := parseJSON(text, v)
	if parseErr == nil {
		return nil
	}
	slog.WarnContext(ctx, "reply was not valid JSON; asking for a repair", logging.AttrComponent, logComponent,
		"raw_preview", logging.Preview(text, 300), logging.AttrError, parseErr)
	call.Prompt += "\n\nYour previous reply could not be parsed as JSON (" + parseErr.Error() + "):\n" +
		text + "\n\nReturn only the corrected JSON object."
	text, err = r.gen(ctx, call)
	if err != nil {
		return err
	}
	if err := parseJSON(text, v); err != nil {
		return &AIError{Msg: "model reply was not valid JSON: " + err.Error(), Invalid: true}
	}
	return nil
}

// parseJSON decodes a model reply into v, tolerating code fences and prose
// around the object.
func parseJSON(text string, v any) error {
	body := jsonstream.StripFences(text)
	if start, end := strings.IndexByte(body, '{'), strings.LastIndexByte(body, '}'); start >= 0 && end > start {
		body = body[start : end+1]
	}
	return json.Unmarshal([]byte(body), v)
}

func withText(segs []Segment) []Segment {
	out := make([]Segment, 0, len(segs))
	for _, s := range segs {
		if strings.TrimSpace(s.Text) != "" {
			out = append(out, s)
		}
	}
	return out
}

func segmentTokens(s Segment) int {
	if s.Tokens > 0 {
		return s.Tokens
	}
	return EstimateTokens(s.Text)
}

// EstimateTokens is the rough token count of text: four characters each.
func EstimateTokens(s string) int {
	return max(1, utf8.RuneCountInString(s)/4)
}

// groups splits segments into consecutive runs of at most size tokens. A
// segment larger than size gets a run of its own.
func groups(segments []Segment, size int) [][]Segment {
	var out [][]Segment
	var cur []Segment
	used := 0
	for _, s := range segments {
		n := segmentTokens(s)
		if len(cur) > 0 && used+n > size {
			out = append(out, cur)
			cur, used = nil, 0
		}
		cur = append(cur, s)
		used += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
