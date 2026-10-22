package branddistill

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/brandcheck"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// CommitVoice, CommitAudience, CommitLine and CommitFact are kept proposal
// items as the user left them, edits included.
type CommitVoice struct {
	ProposalID   string
	Name         string
	WhenToUse    string
	IsDefault    bool
	Samples      []string
	Rules        models.VoiceRules
	ChannelNotes map[string]string
}

type CommitAudience struct {
	ProposalID      string
	Name            string
	Who             string
	ReadsOn         string
	ScrollsPastWhen string
	BelievesWhen    string
}

type CommitLine struct {
	ProposalID string
	Text       string
}

type CommitFact struct {
	ProposalID string
	Statement  string
	Subject    models.FactSubject
	Kind       models.FactKind
	ExpiresAt  *models.CalendarDate
	Source     string
}

// CommitInput is what the user keeps of a ready run. Items not sent are not
// saved; the disclaimer replaces the workspace's own only when sent.
type CommitInput struct {
	TenantID    string
	RunID       string
	UserID      *string
	Voices      []CommitVoice
	Audiences   []CommitAudience
	MayClaim    []CommitLine
	NeverClaim  []CommitLine
	BannedWords []CommitLine
	Disclaimer  *CommitLine
	Facts       []CommitFact
}

// CommitResult is what a commit saved and skipped, with the run.
type CommitResult struct {
	Run    *models.BrandDistillRun
	Result *repository.DistilledResult
}

// Commit saves the kept items in one transaction and marks the run
// committed. Only a ready run commits; an item must be one the proposal
// holds, and passes the same checks as the Brand editor.
func (s *Service) Commit(ctx context.Context, in CommitInput) (*CommitResult, error) {
	ctx = tenantctx.With(ctx, in.TenantID)
	run, err := s.repos.Runs.Get(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	if err := commitable(run); err != nil {
		return nil, err
	}
	batch, err := buildBatch(run, in)
	if err != nil {
		return nil, err
	}
	var committed *models.BrandDistillRun
	res, err := s.repos.Brands.ApplyDistilled(ctx, batch, func(ctx context.Context, tx bun.Tx, res *repository.DistilledResult) error {
		locked, err := s.repos.Runs.Lock(ctx, tx, in.RunID)
		if err != nil {
			return err
		}
		if err := commitable(locked); err != nil {
			return err
		}
		now := time.Now().UTC()
		locked.Status, locked.CommittedBy, locked.CommittedAt = models.BrandDistillStatusCommitted, in.UserID, &now
		locked.CommittedIDs = models.StringMap{}
		for _, m := range []map[string]string{res.Voices, res.Audiences, res.Facts} {
			maps.Copy(locked.CommittedIDs, m)
		}
		for _, id := range res.Guardrails {
			locked.CommittedIDs[id] = "guardrails"
		}
		committed = locked
		return s.repos.Runs.Save(ctx, tx, locked, "status", "committed_by", "committed_at", "committed_ids")
	})
	if err != nil {
		return nil, err
	}
	committedTotal.Add("voices", int64(len(res.Voices)))
	committedTotal.Add("audiences", int64(len(res.Audiences)))
	committedTotal.Add("guardrails", int64(len(res.Guardrails)))
	committedTotal.Add("facts", int64(len(res.Facts)))
	committedTotal.Add("skipped", int64(len(res.Skipped)))
	runsTotal.Add("committed", 1)
	s.publish(ctx, committed)
	return &CommitResult{Run: committed, Result: res}, nil
}

func commitable(run *models.BrandDistillRun) error {
	switch run.Status {
	case models.BrandDistillStatusReady:
		if run.Proposal == nil {
			return ErrNotReady
		}
		return nil
	case models.BrandDistillStatusCommitted:
		return ErrCommitted
	}
	return ErrNotReady
}

// proposalIndex finds a proposal's items by id.
type proposalIndex struct {
	voices     map[string]models.ProposedVoice
	audiences  map[string]models.ProposedAudience
	lines      map[string]models.ProposedLine
	disclaimer *models.ProposedDisclaimer
	facts      map[string]models.ProposedFact
}

func indexProposal(p *models.BrandProposal) proposalIndex {
	ix := proposalIndex{
		voices: map[string]models.ProposedVoice{}, audiences: map[string]models.ProposedAudience{},
		lines: map[string]models.ProposedLine{}, facts: map[string]models.ProposedFact{},
		disclaimer: p.Guardrails.Disclaimer,
	}
	for _, v := range p.Voices {
		ix.voices[v.ProposalID] = v
	}
	for _, a := range p.Audiences {
		ix.audiences[a.ProposalID] = a
	}
	for _, list := range guardrailLists(p) {
		for _, l := range list {
			ix.lines[l.ProposalID] = l
		}
	}
	for _, f := range p.Facts {
		ix.facts[f.ProposalID] = f
	}
	return ix
}

// buildBatch checks the kept items against the proposal and the brand rules
// and turns them into the brand entries to create, each with its origin.
func buildBatch(run *models.BrandDistillRun, in CommitInput) (repository.DistilledBrand, error) {
	ix := indexProposal(run.Proposal)
	batch := repository.DistilledBrand{Author: in.UserID}
	invalid := func(id string, err error) error {
		return &InputError{Code: CodeInvalidItem, Msg: err.Error(), ProposalID: id}
	}
	unknown := func(id string) error {
		return &InputError{Code: CodeUnknownItem, Msg: "the proposal has no item " + id, ProposalID: id}
	}
	for _, v := range in.Voices {
		p, ok := ix.voices[v.ProposalID]
		if !ok {
			return batch, unknown(v.ProposalID)
		}
		voice := models.BrandVoice{
			Name: strings.TrimSpace(v.Name), WhenToUse: strings.TrimSpace(v.WhenToUse), Samples: trimmed(v.Samples),
			Rules: v.Rules, ChannelNotes: models.StringMap(v.ChannelNotes), Origin: originOf(run, append(sampleEvidence(p), p.Evidence...)),
		}
		if voice.ChannelNotes == nil {
			voice.ChannelNotes = models.StringMap{}
		}
		if err := brandcheck.Voice(&voice); err != nil {
			return batch, invalid(v.ProposalID, err)
		}
		batch.Voices = append(batch.Voices, repository.DistilledVoice{ProposalID: v.ProposalID, Voice: voice, WantDefault: v.IsDefault})
	}
	for _, a := range in.Audiences {
		p, ok := ix.audiences[a.ProposalID]
		if !ok {
			return batch, unknown(a.ProposalID)
		}
		aud := models.BrandAudience{
			Name: strings.TrimSpace(a.Name), Who: strings.TrimSpace(a.Who), ReadsOn: strings.TrimSpace(a.ReadsOn),
			ScrollsPastWhen: strings.TrimSpace(a.ScrollsPastWhen), BelievesWhen: strings.TrimSpace(a.BelievesWhen),
			Origin: originOf(run, p.Evidence),
		}
		if err := brandcheck.Audience(&aud); err != nil {
			return batch, invalid(a.ProposalID, err)
		}
		batch.Audiences = append(batch.Audiences, repository.DistilledAudience{ProposalID: a.ProposalID, Audience: aud})
	}
	lines := func(in []CommitLine, prefix string, limit int) ([]repository.DistilledLine, error) {
		out := make([]repository.DistilledLine, 0, len(in))
		for _, l := range in {
			if _, ok := ix.lines[l.ProposalID]; !ok || !strings.HasPrefix(l.ProposalID, prefix) {
				return nil, unknown(l.ProposalID)
			}
			text := strings.TrimSpace(l.Text)
			if text == "" || len(text) > limit {
				return nil, invalid(l.ProposalID, fmt.Errorf("a line is required and at most %d KB", brandcheck.MaxStatementBytes>>10))
			}
			out = append(out, repository.DistilledLine{ProposalID: l.ProposalID, Text: text})
		}
		return out, nil
	}
	var err error
	if batch.MayClaim, err = lines(in.MayClaim, "g:mayClaim:", brandcheck.MaxStatementBytes); err != nil {
		return batch, err
	}
	if batch.NeverClaim, err = lines(in.NeverClaim, "g:neverClaim:", brandcheck.MaxStatementBytes); err != nil {
		return batch, err
	}
	if batch.BannedWords, err = lines(in.BannedWords, "g:bannedWords:", brandcheck.MaxStatementBytes); err != nil {
		return batch, err
	}
	if d := in.Disclaimer; d != nil {
		if ix.disclaimer == nil || d.ProposalID != ix.disclaimer.ProposalID {
			return batch, unknown(d.ProposalID)
		}
		text := strings.TrimSpace(d.Text)
		if text == "" || len(text) > brandcheck.MaxDisclaimerBytes {
			return batch, invalid(d.ProposalID, fmt.Errorf("the disclaimer is required and at most %d KB", brandcheck.MaxDisclaimerBytes>>10))
		}
		batch.Disclaimer = &repository.DistilledLine{ProposalID: d.ProposalID, Text: text}
	}
	today := models.CalendarDate{Time: time.Now().UTC().Truncate(24 * time.Hour)}
	for _, f := range in.Facts {
		if _, ok := ix.facts[f.ProposalID]; !ok {
			return batch, unknown(f.ProposalID)
		}
		fact := models.BrandFact{
			Statement: strings.TrimSpace(f.Statement), Subject: f.Subject, Kind: f.Kind,
			Source: strings.TrimSpace(f.Source), ExpiresAt: f.ExpiresAt, AddedAt: &today, CheckedAt: &today,
		}
		if err := brandcheck.Fact(&fact); err != nil {
			return batch, invalid(f.ProposalID, err)
		}
		batch.Facts = append(batch.Facts, repository.DistilledFact{ProposalID: f.ProposalID, Fact: fact})
	}
	if len(batch.Voices)+len(batch.Audiences)+len(batch.MayClaim)+len(batch.NeverClaim)+len(batch.BannedWords)+len(batch.Facts) == 0 && batch.Disclaimer == nil {
		return batch, &InputError{Code: CodeNothingToSave, Msg: "keep at least one item to save"}
	}
	return batch, nil
}

func sampleEvidence(v models.ProposedVoice) []models.DistillEvidence {
	out := make([]models.DistillEvidence, 0, len(v.Samples))
	for _, s := range v.Samples {
		out = append(out, s.Evidence)
	}
	return out
}

// originOf is where a committed entry came from, read off its first piece of
// evidence: a page the website read imported, another asset, or the answers.
func originOf(run *models.BrandDistillRun, evidence []models.DistillEvidence) models.BrandOrigin {
	for _, e := range evidence {
		if e.Kind == models.EvidenceKindAnswer || e.AssetID == "" {
			continue
		}
		for _, src := range run.Sources {
			if src.AssetID != e.AssetID {
				continue
			}
			if src.Role == models.DistillRoleWebsite && run.Website != nil {
				return models.BrandOrigin{Kind: models.BrandOriginWebsite, URL: run.Website.URL, RunID: run.ID}
			}
			return models.BrandOrigin{Kind: models.BrandOriginDocument, AssetID: src.AssetID, Title: src.Title, RunID: run.ID}
		}
	}
	return models.BrandOrigin{Kind: models.BrandOriginQuestions, RunID: run.ID}
}

func trimmed(in []string) models.StringSlice {
	out := models.StringSlice{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
