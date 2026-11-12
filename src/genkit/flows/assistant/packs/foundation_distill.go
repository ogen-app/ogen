package packs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
)

// Distiller reads a brand off a website, documents or answers;
// branddistill.Service.
type Distiller interface {
	Request(ctx context.Context, in branddistill.RequestInput) (*models.BrandDistillRun, error)
	Get(ctx context.Context, tenantID, runID string) (*models.BrandDistillRun, error)
	List(ctx context.Context, tenantID string) ([]models.BrandDistillRun, error)
	Refine(ctx context.Context, tenantID, runID string, userID *string, answers []branddistill.Answer) (*models.BrandDistillRun, error)
	Commit(ctx context.Context, in branddistill.CommitInput) (*branddistill.CommitResult, error)
}

// DistillRef names a run; the latest when empty.
type DistillRef struct {
	RunID string `json:"runId,omitempty" jsonschema:"description=The run; the latest when omitted"`
}

// DistillItem is one proposed item: its id to keep it by, and what it says.
type DistillItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind" jsonschema:"description=voice, audience, mayClaim, neverClaim, bannedWord, disclaimer or fact"`
	Text string `json:"text"`
}

// DistillOut is a run: where it is, what it proposes and what it asks.
type DistillOut struct {
	RunID  string `json:"runId"`
	Status string `json:"status"`
	// Failure is why a failed run failed.
	Failure *branddistill.Failure `json:"failure,omitempty"`
	// Superseded runs were answered: RefinedBy is the run that replaced it.
	RefinedBy string                            `json:"refinedBy,omitempty"`
	Counts    branddistill.Counts               `json:"counts"`
	Items     []DistillItem                     `json:"items,omitempty"`
	Questions []models.DistillQuestion          `json:"questions,omitempty"`
	Coverage  map[string]models.DistillCoverage `json:"coverage,omitempty"`
	// Sources is what the run reads.
	Sources DistillSources `json:"sources"`
	// Answered is the questions an answerDistillQuestions call answered, with
	// where each answer came from; Grounding names the assets they cite.
	Answered  []DistillAnswered       `json:"answered,omitempty"`
	Grounding *assistantkit.Grounding `json:"grounding,omitempty"`
}

// DistillSources is what a run reads: a website, by its address, with the
// pages read off it, and assets.
type DistillSources struct {
	Website string `json:"website,omitempty"`
	Pages   int    `json:"pages,omitzero"`
	Assets  int    `json:"assets"`
}

// DistillAnswered is a question answered, and where the answer came from.
type DistillAnswered struct {
	QuestionID string `json:"questionId"`
	Question   string `json:"question"`
	Answer     string `json:"answer"`
	From       string `json:"from,omitempty"`
}

// StartDistillInput starts a run.
type StartDistillInput struct {
	WebsiteURL string   `json:"websiteUrl,omitempty"`
	AssetIDs   []string `json:"assetIds,omitempty" jsonschema:"description=Assets to read; the open asset when none are given and no website is"`
}

// DistillAnswerInput is one answered question.
type DistillAnswerInput struct {
	QuestionID string `json:"questionId"`
	Answer     string `json:"answer"`
	// From and AssetIDs are reported, not sent to the reading.
	From     string   `json:"from,omitempty"     jsonschema:"description=Where the answer came from, e.g. the brochure, p. 2 or the guardrails; empty when the user said it"`
	AssetIDs []string `json:"assetIds,omitempty" jsonschema:"description=The assets the answer came from"`
}

// AnswerDistillInput answers a ready run's questions.
type AnswerDistillInput struct {
	RunID   string               `json:"runId,omitempty" jsonschema:"description=The run; the latest when omitted"`
	Answers []DistillAnswerInput `json:"answers"`
}

// CommitDistillInput saves a ready run's proposal: every item, or only those
// kept, less those skipped.
type CommitDistillInput struct {
	RunID string   `json:"runId,omitempty" jsonschema:"description=The run; the latest when omitted"`
	Keep  []string `json:"keep,omitempty"  jsonschema:"description=Item ids to save; every item when empty"`
	Skip  []string `json:"skip,omitempty"  jsonschema:"description=Item ids to leave out"`
}

// DistillSaved is what a commit saved and skipped.
type DistillSaved struct {
	RunID      string   `json:"runId"`
	Voices     int      `json:"voices"`
	Audiences  int      `json:"audiences"`
	Guardrails int      `json:"guardrails"`
	Facts      int      `json:"facts"`
	Skipped    []string `json:"skipped,omitempty"`
}

func distillErr(err error) error {
	if in, ok := errors.AsType[*branddistill.InputError](err); ok {
		return assistantkit.Refused(in.Msg)
	}
	if q, ok := errors.AsType[*entitlements.QuotaExceededError](err); ok {
		return assistantkit.Refused(fmt.Sprintf("the plan's %d brand readings this month are used up", q.Limit))
	}
	if _, ok := errors.AsType[*entitlements.FeatureNotAvailableError](err); ok {
		return assistantkit.Refused("reading the brand isn't on this plan")
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return assistantkit.Refused("there's no such brand reading")
	case errors.Is(err, branddistill.ErrInProgress):
		return assistantkit.Refused("the brand is already being read; check on it with getBrandDistill")
	case errors.Is(err, branddistill.ErrScrapingUnavailable):
		return assistantkit.Refused("reading websites isn't set up here; give documents or answers instead")
	case errors.Is(err, branddistill.ErrNotReady):
		return assistantkit.Refused("that reading has nothing waiting to save or answer yet")
	case errors.Is(err, branddistill.ErrCommitted):
		return assistantkit.Refused("that reading was saved already")
	case errors.Is(err, branddistill.ErrRefineLimit):
		return assistantkit.Refused("no more answer rounds for this reading; start a new one")
	case errors.Is(err, branddistill.ErrUnavailable):
		return assistantkit.Refused("reading the brand isn't available right now")
	}
	return err
}

func tenantOf(ctx context.Context) string {
	t, _ := tenantctx.From(ctx)
	return t
}

// latestRun is run id, or the newest run that wasn't answered by another.
func latestRun(ctx context.Context, d Deps, id string) (*models.BrandDistillRun, error) {
	if id = strings.TrimSpace(id); id != "" {
		run, err := d.Distill.Get(ctx, tenantOf(ctx), id)
		return run, distillErr(err)
	}
	runs, err := d.Distill.List(ctx, tenantOf(ctx))
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.Status != models.BrandDistillStatusSuperseded {
			// The list leaves proposals out.
			run, err := d.Distill.Get(ctx, tenantOf(ctx), r.ID)
			return run, distillErr(err)
		}
	}
	return nil, assistantkit.Refused("the brand hasn't been read yet; start a reading with startBrandDistill")
}

func distillOut(run *models.BrandDistillRun) *DistillOut {
	out := &DistillOut{RunID: run.ID, Status: string(run.Status), Failure: branddistill.FailureOf(run), Counts: branddistill.CountsOf(run.Proposal),
		Sources: distillSources(run)}
	if run.ChildRunID != nil {
		out.RefinedBy = *run.ChildRunID
	}
	if p := run.Proposal; p != nil {
		out.Items, out.Questions, out.Coverage = proposalItems(p), p.Questions, p.Coverage
	}
	return out
}

func proposalItems(p *models.BrandProposal) []DistillItem {
	var out []DistillItem
	for _, v := range p.Voices {
		out = append(out, DistillItem{ID: v.ProposalID, Kind: "voice", Text: v.Name + " — " + v.WhenToUse})
	}
	for _, a := range p.Audiences {
		out = append(out, DistillItem{ID: a.ProposalID, Kind: "audience", Text: a.Name + " — " + a.Who})
	}
	for kind, lines := range map[string][]models.ProposedLine{"mayClaim": p.Guardrails.MayClaim, "neverClaim": p.Guardrails.NeverClaim, "bannedWord": p.Guardrails.BannedWords} {
		for _, l := range lines {
			out = append(out, DistillItem{ID: l.ProposalID, Kind: kind, Text: l.Text})
		}
	}
	if dc := p.Guardrails.Disclaimer; dc != nil {
		out = append(out, DistillItem{ID: dc.ProposalID, Kind: "disclaimer", Text: dc.Text})
	}
	for _, f := range p.Facts {
		out = append(out, DistillItem{ID: f.ProposalID, Kind: "fact", Text: f.Statement})
	}
	slices.SortStableFunc(out, func(a, b DistillItem) int { return strings.Compare(a.Kind, b.Kind) })
	return out
}

func getBrandDistill(ctx context.Context, d Deps, in DistillRef) (*DistillOut, error) {
	run, err := latestRun(ctx, d, in.RunID)
	if err != nil {
		return nil, err
	}
	return distillOut(run), nil
}

func startBrandDistill(ctx context.Context, d Deps, in StartDistillInput) (*DistillOut, error) {
	ids := in.AssetIDs
	if len(ids) == 0 && strings.TrimSpace(in.WebsiteURL) == "" {
		if open := assistant.FocusedAsset(ctx); open != "" {
			ids = []string{open}
		}
	}
	by := actor(ctx)
	run, err := d.Distill.Request(ctx, branddistill.RequestInput{TenantID: tenantOf(ctx), UserID: &by, WebsiteURL: in.WebsiteURL, AssetIDs: ids})
	if err != nil {
		return nil, distillErr(err)
	}
	assistantkit.Touch(ctx, "brand_distill", run.ID)
	return distillOut(run), nil
}

func answerDistill(ctx context.Context, d Deps, in AnswerDistillInput) (*DistillOut, error) {
	run, err := latestRun(ctx, d, in.RunID)
	if err != nil {
		return nil, err
	}
	asked := map[string]string{}
	if run.Proposal != nil {
		for _, q := range run.Proposal.Questions {
			asked[q.ID] = q.Text
		}
	}
	answers := make([]branddistill.Answer, 0, len(in.Answers))
	for _, a := range in.Answers {
		text, ok := asked[a.QuestionID]
		if !ok {
			return nil, assistantkit.Refused("question " + a.QuestionID + " isn't one this reading asks; read them with getBrandDistill")
		}
		answers = append(answers, branddistill.Answer{QuestionID: a.QuestionID, Question: text, Answer: a.Answer})
	}
	by := actor(ctx)
	child, err := d.Distill.Refine(ctx, tenantOf(ctx), run.ID, &by, answers)
	if err != nil {
		return nil, distillErr(err)
	}
	assistantkit.Touch(ctx, "brand_distill", child.ID)
	out := distillOut(child)
	var cited []string
	for i, a := range in.Answers {
		out.Answered = append(out.Answered, DistillAnswered{QuestionID: a.QuestionID, Question: answers[i].Question,
			Answer: a.Answer, From: strings.TrimSpace(a.From)})
		for _, id := range trimAll(a.AssetIDs) {
			if !slices.Contains(cited, id) {
				cited = append(cited, id)
			}
		}
	}
	out.Grounding = new(newAssetTitles(d).grounding(ctx, fromIdeas(cited, nil)))
	return out, nil
}

// distillSources counts what run reads. A website's address is the one
// asked for, or the one mapped.
func distillSources(run *models.BrandDistillRun) DistillSources {
	var out DistillSources
	if run.WebsiteURL != nil {
		out.Website = *run.WebsiteURL
	} else if run.Website != nil {
		out.Website = run.Website.URL
	}
	for _, src := range run.Sources {
		switch src.Role {
		case models.DistillRoleWebsite:
			out.Pages++
		case models.DistillRoleAsset:
			out.Assets++
		}
	}
	return out
}

// keeps reports whether item id is saved.
func (in CommitDistillInput) keeps(id string) bool {
	return (len(in.Keep) == 0 || slices.Contains(in.Keep, id)) && !slices.Contains(in.Skip, id)
}

// commitInput is the proposal's kept items, as proposed. The disclaimer
// replaces the workspace's own, so keeping everything saves it only when the
// workspace has none; naming it in keep saves it either way.
func (in CommitDistillInput) commitInput(p *models.BrandProposal) branddistill.CommitInput {
	var out branddistill.CommitInput
	for _, v := range p.Voices {
		if in.keeps(v.ProposalID) {
			samples := make([]string, len(v.Samples))
			for i, s := range v.Samples {
				samples[i] = s.Text
			}
			out.Voices = append(out.Voices, branddistill.CommitVoice{ProposalID: v.ProposalID, Name: v.Name, WhenToUse: v.WhenToUse,
				IsDefault: v.IsDefault, Samples: samples, Rules: v.Rules, ChannelNotes: v.ChannelNotes})
		}
	}
	for _, a := range p.Audiences {
		if in.keeps(a.ProposalID) {
			out.Audiences = append(out.Audiences, branddistill.CommitAudience{ProposalID: a.ProposalID, Name: a.Name, Who: a.Who,
				ReadsOn: a.ReadsOn, ScrollsPastWhen: a.ScrollsPastWhen, BelievesWhen: a.BelievesWhen})
		}
	}
	lines := func(src []models.ProposedLine) []branddistill.CommitLine {
		var kept []branddistill.CommitLine
		for _, l := range src {
			if in.keeps(l.ProposalID) {
				kept = append(kept, branddistill.CommitLine{ProposalID: l.ProposalID, Text: l.Text})
			}
		}
		return kept
	}
	out.MayClaim, out.NeverClaim, out.BannedWords = lines(p.Guardrails.MayClaim), lines(p.Guardrails.NeverClaim), lines(p.Guardrails.BannedWords)
	if dc := p.Guardrails.Disclaimer; dc != nil && in.keeps(dc.ProposalID) && (dc.Current == nil || slices.Contains(in.Keep, dc.ProposalID)) {
		out.Disclaimer = &branddistill.CommitLine{ProposalID: dc.ProposalID, Text: dc.Text}
	}
	for _, f := range p.Facts {
		if in.keeps(f.ProposalID) {
			out.Facts = append(out.Facts, branddistill.CommitFact{ProposalID: f.ProposalID, Statement: f.Statement, Subject: f.Subject,
				Kind: f.Kind, ExpiresAt: f.ExpiresAt, Source: f.Source})
		}
	}
	return out
}

func commitDistill(ctx context.Context, d Deps, in CommitDistillInput) (*DistillSaved, error) {
	run, err := latestRun(ctx, d, in.RunID)
	if err != nil {
		return nil, err
	}
	if run.Proposal == nil {
		return nil, distillErr(branddistill.ErrNotReady)
	}
	ci := in.commitInput(run.Proposal)
	by := actor(ctx)
	ci.TenantID, ci.RunID, ci.UserID = tenantOf(ctx), run.ID, &by
	res, err := d.Distill.Commit(ctx, ci)
	if err != nil {
		return nil, distillErr(err)
	}
	r := res.Result
	out := &DistillSaved{RunID: run.ID, Voices: len(r.Voices), Audiences: len(r.Audiences), Guardrails: len(r.Guardrails), Facts: len(r.Facts)}
	for _, s := range r.Skipped {
		out.Skipped = append(out.Skipped, s.ProposalID)
	}
	for _, k := range []string{"voice", "audience", "fact", "guardrails"} {
		assistantkit.Touch(ctx, k, "")
	}
	return out, nil
}

// distillSpecs are the brand reading tools; nil without the distill service.
func distillSpecs(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	if d.Distill == nil {
		return nil
	}
	get := genkit.DefineTool(g, "getBrandDistill",
		"Returns a brand reading: where it is (pending, collecting, reading, composing, ready, committed, failed), what it proposes — voices, audiences, guardrail lines, facts, each with an id — and the questions it asks about what the sources left out.",
		assistantkit.Wrap(recorded(foundationPackName, "getBrandDistill", nil, func(ctx context.Context, in DistillRef) (*DistillOut, error) { return getBrandDistill(ctx, d, in) })), ai.WithStrictSchema(false))
	start := genkit.DefineTool(g, "startBrandDistill",
		"Starts reading the brand off a website (its homepage and up to nine pages) and/or assets, proposing voices, audiences, guardrails and facts to save. Always offered for the user to confirm first; it counts against the plan's monthly readings.",
		assistantkit.Gated("startBrandDistill", assistantkit.Gate[StartDistillInput]{
			Summary: func(in StartDistillInput) string {
				switch {
				case in.WebsiteURL != "" && len(in.AssetIDs) > 0:
					return fmt.Sprintf("Read the brand off %s and %s", in.WebsiteURL, plural(len(in.AssetIDs), "asset", "assets"))
				case in.WebsiteURL != "":
					return "Read the brand off " + in.WebsiteURL
				case len(in.AssetIDs) > 0:
					return "Read the brand off " + plural(len(in.AssetIDs), "asset", "assets")
				}
				return "Read the brand off the open asset"
			},
		}, assistantkit.Wrap(recorded(foundationPackName, "startBrandDistill", nil, func(ctx context.Context, in StartDistillInput) (*DistillOut, error) {
			return startBrandDistill(ctx, d, in)
		}))),
		ai.WithStrictSchema(false))
	answer := genkit.DefineTool(g, "answerDistillQuestions",
		"Answers a ready reading's questions; the proposal is composed again with the answers. Always offered for the user to confirm first.",
		assistantkit.Gated("answerDistillQuestions", assistantkit.Gate[AnswerDistillInput]{
			Precheck: func(_ context.Context, in AnswerDistillInput) error {
				if len(in.Answers) == 0 {
					return assistantkit.Refused("no answers given")
				}
				return nil
			},
			Summary: func(in AnswerDistillInput) string {
				return "Answer " + plural(len(in.Answers), "question", "questions") + " and update the brand proposal"
			},
		}, assistantkit.Wrap(recorded(foundationPackName, "answerDistillQuestions", nil, func(ctx context.Context, in AnswerDistillInput) (*DistillOut, error) {
			return answerDistill(ctx, d, in)
		}))),
		ai.WithStrictSchema(false))
	commit := genkit.DefineTool(g, "commitBrandDistill",
		"Saves a ready reading's proposal to the brand: every item, or the ones kept. Nothing existing is changed or removed. Always offered for the user to confirm first.",
		assistantkit.Gated("commitBrandDistill", assistantkit.Gate[CommitDistillInput]{
			Summary: func(in CommitDistillInput) string {
				switch {
				case len(in.Keep) > 0:
					return "Save " + plural(len(in.Keep), "proposed item", "proposed items") + " to the brand"
				case len(in.Skip) > 0:
					return "Save the brand proposal, less " + plural(len(in.Skip), "item", "items")
				}
				return "Save the brand proposal"
			},
		}, assistantkit.Wrap(recorded(foundationPackName, "commitBrandDistill", nil, func(ctx context.Context, in CommitDistillInput) (*DistillSaved, error) {
			return commitDistill(ctx, d, in)
		}))),
		ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(foundationPackName, get, assistantkit.TierRead,
			`Call this when the user asks how reading their brand is going, what it found, or what it asks — "what questions does the brand reading have?".`),
		spec(foundationPackName, start, assistantkit.TierProposal,
			`Call this when the user asks to read their brand off their website or documents — "fill in our brand from acme.com", "learn our voice from these assets". It runs in the background; check it with getBrandDistill.`),
		spec(foundationPackName, answer, assistantkit.TierProposal,
			`Call this when the user answers the reading's questions, or asks you to answer what you can from what the workspace has. Pass each answer with its questionId from getBrandDistill; use the user's words. For an answer you found, say where in from and pass the assets in assetIds; leave a question nothing answers out and name it, never guess.`),
		spec(foundationPackName, commit, assistantkit.TierProposal,
			`Call this when the user wants to keep what the reading proposes — all of it, or the items they name (by id from getBrandDistill).`),
	}
}
