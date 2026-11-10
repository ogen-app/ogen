package assistantundo

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// IdeaVerdict is an idea's triage state, as an undo log stores it: the
// verdict, its reminder, and who decided it when.
func IdeaVerdict(i *models.Idea) models.JSONMap {
	m := models.JSONMap{"verdict": nil, "remindAt": timeValue(i.RemindAt), "decidedAt": timeValue(i.DecidedAt), "decidedBy": nil}
	if i.Verdict != nil {
		m["verdict"] = string(*i.Verdict)
	}
	if i.DecidedBy != nil {
		m["decidedBy"] = *i.DecidedBy
	}
	return m
}

// ideaTriage reads an IdeaVerdict snapshot back onto an idea.
func ideaTriage(id string, m models.JSONMap) (*models.Idea, error) {
	i := &models.Idea{ID: id}
	if v, ok := m["verdict"].(string); ok && v != "" {
		i.Verdict = new(models.IdeaVerdict(v))
	}
	if by, ok := m["decidedBy"].(string); ok {
		i.DecidedBy = &by
	}
	var err error
	if i.RemindAt, err = parseTime(m["remindAt"]); err != nil {
		return nil, fmt.Errorf("remind_at: %w", err)
	}
	if i.DecidedAt, err = parseTime(m["decidedAt"]); err != nil {
		return nil, fmt.Errorf("decided_at: %w", err)
	}
	return i, nil
}

// CampaignAssets is the assets a campaign draws on, as an undo log stores
// them, with whether it draws on them at all.
func CampaignAssets(c *models.Campaign) models.JSONMap {
	ids := make([]any, len(c.AssetIDs))
	for i, id := range c.AssetIDs {
		ids[i] = id
	}
	return models.JSONMap{"assetIds": ids, "useAssets": c.UseAssets}
}

// assetIDs reads a CampaignAssets snapshot's ids, without repeats.
func assetIDs(m models.JSONMap) []string {
	raw, _ := m["assetIds"].([]any)
	ids := []string{}
	for _, v := range raw {
		if s, ok := v.(string); ok && !slices.Contains(ids, s) {
			ids = append(ids, s)
		}
	}
	return ids
}

// IdeaEdit is an idea's title, note and plan, as an undo log stores them.
func IdeaEdit(i *models.Idea) models.JSONMap {
	return toMap(ideaText{Title: i.Title, Note: i.Note, Plan: i.Plan})
}

// ideaText is what IdeaEdit stores.
type ideaText struct {
	Title string          `json:"title"`
	Note  string          `json:"note"`
	Plan  models.IdeaPlan `json:"plan"`
}

// ideaEdited reads an IdeaEdit snapshot back onto an idea.
func ideaEdited(id string, m models.JSONMap) (*models.Idea, error) {
	var t ideaText
	if err := fromMap(m, &t); err != nil {
		return nil, err
	}
	return &models.Idea{ID: id, Title: t.Title, Note: t.Note, Plan: t.Plan}, nil
}

// IdeaVerdicts is several ideas' triage states, as an undo log stores them,
// in the same order before and after a change.
func IdeaVerdicts(ideas []*models.Idea) models.JSONMap {
	list := make([]any, len(ideas))
	for i, idea := range ideas {
		m := IdeaVerdict(idea)
		m["id"] = idea.ID
		list[i] = map[string]any(m)
	}
	return models.JSONMap{"ideas": list}
}

// ideaTriages reads an IdeaVerdicts snapshot back onto its ideas.
func ideaTriages(m models.JSONMap) ([]*models.Idea, error) {
	raw, _ := m["ideas"].([]any)
	out := make([]*models.Idea, len(raw))
	for i, r := range raw {
		one, _ := r.(map[string]any)
		id, _ := one["id"].(string)
		idea, err := ideaTriage(id, one)
		if err != nil {
			return nil, err
		}
		out[i] = idea
	}
	return out, nil
}

// WithIdeas lets s undo idea verdict changes, one or several at once, and
// idea edits.
func (s *Service) WithIdeas(ideas repository.IdeaRepository) *Service {
	s.undoers[models.AssistantActionIdeaVerdict] = ideaVerdict{ideas}
	s.undoers[models.AssistantActionIdeaVerdicts] = ideaVerdicts{ideas}
	s.undoers[models.AssistantActionIdeaEdit] = ideaEdit{ideas}
	return s
}

type ideaVerdicts struct{ ideas repository.IdeaRepository }

// Undo restores every idea's triage, all of them or none: one triaged,
// edited or deleted since leaves the change in place.
func (u ideaVerdicts) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	was, err := ideaTriages(a.After)
	if err != nil {
		return fmt.Errorf("read changed triage: %w", err)
	}
	restored, err := ideaTriages(a.Before)
	if err != nil {
		return fmt.Errorf("restore triage: %w", err)
	}
	if len(was) != len(restored) {
		return ErrChangedSince
	}
	now := time.Now().UTC()
	for i := range restored {
		if restored[i].ID != was[i].ID {
			return ErrChangedSince
		}
		restored[i].UpdatedAt = now
		ok, err := u.ideas.SetTriageIf(ctx, tx, restored[i], was[i])
		if err != nil {
			return err
		}
		if !ok {
			return ErrChangedSince // rolls back the ones restored
		}
	}
	return nil
}

type ideaEdit struct{ ideas repository.IdeaRepository }

// Undo restores the idea's title, note and plan within tx, in one write
// that matches only while the idea still holds the logged after state.
func (u ideaEdit) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	was, err := ideaEdited(a.EntityID, a.After)
	if err != nil {
		return fmt.Errorf("read changed idea: %w", err)
	}
	restored, err := ideaEdited(a.EntityID, a.Before)
	if err != nil {
		return fmt.Errorf("restore idea: %w", err)
	}
	restored.UpdatedAt = time.Now().UTC()
	ok, err := u.ideas.SetEditIf(ctx, tx, restored, was)
	if err != nil {
		return err
	}
	if !ok {
		return ErrChangedSince
	}
	return nil
}

type ideaVerdict struct{ ideas repository.IdeaRepository }

// Undo restores the idea's verdict, reminder and decision within tx, in one
// write that matches only while the idea still holds the logged after state.
func (u ideaVerdict) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	was, err := ideaTriage(a.EntityID, a.After)
	if err != nil {
		return fmt.Errorf("read changed triage: %w", err)
	}
	restored, err := ideaTriage(a.EntityID, a.Before)
	if err != nil {
		return fmt.Errorf("restore triage: %w", err)
	}
	restored.UpdatedAt = time.Now().UTC()
	ok, err := u.ideas.SetTriageIf(ctx, tx, restored, was)
	if err != nil {
		return err
	}
	if !ok {
		return ErrChangedSince
	}
	return nil
}

type campaignAssets struct{ campaigns repository.CampaignRepository }

// Undo restores the campaign's assets and whether it draws on them within
// tx, writing only those columns and only while they still hold the logged
// after state.
func (u campaignAssets) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	before := assetIDs(a.Before)
	use, ok := a.Before["useAssets"].(bool)
	if !ok {
		use = len(before) > 0
	}
	wasUse, _ := a.After["useAssets"].(bool)
	restored, err := u.campaigns.SetAssetIDsIf(ctx, tx, a.EntityID, assetIDs(a.After), wasUse, before, use)
	if err != nil {
		return err
	}
	if !restored {
		return ErrChangedSince
	}
	return nil
}
