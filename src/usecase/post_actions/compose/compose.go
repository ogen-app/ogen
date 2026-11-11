// Package compose turns ideas into posts. Compile gathers one or more ideas
// into a planned post per platform: their titles and angles become its
// theses, their plan suggests where it lands, and the post remembers which
// ideas it came from.
package compose

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/post_actions/internal/postaudit"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// MaxIdeas caps the ideas one post is compiled from.
const MaxIdeas = 10

// maxTitle caps a compiled post's title, the length of an idea title.
const maxTitle = 300

// Input errors, all a 400.
var (
	ErrIdeaCount      = fmt.Errorf("pick between 1 and %d ideas", MaxIdeas)
	ErrIdeaNotFound   = errors.New("one of the ideas does not exist")
	ErrNoCampaign     = errors.New("the ideas are not in a campaign; pick the campaign for the post")
	ErrMixedCampaigns = errors.New("the ideas are in different campaigns; pick the campaign for the post")
	ErrCampaignGone   = errors.New("campaign not found")
	ErrNotTarget      = errors.New("that platform is not one of the campaign's platforms")
	ErrPostType       = errors.New("the campaign doesn't use that post type on that platform")
	ErrPostTypeAlone  = errors.New("a post type needs its platform")
	ErrInvalidPhase   = errors.New("that phase is not one of the campaign's")
	ErrTitleTooLong   = fmt.Errorf("the title is longer than %d characters", maxTitle)
)

// IsValidation reports whether err is a 400-worthy input error.
func IsValidation(err error) bool {
	for _, target := range []error{
		ErrIdeaCount, ErrIdeaNotFound, ErrNoCampaign, ErrMixedCampaigns, ErrCampaignGone,
		ErrNotTarget, ErrPostType, ErrPostTypeAlone, ErrInvalidPhase, ErrTitleTooLong,
		ErrNoPlatform, ErrTooManyDrafts, ErrPostTypeOneFor,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// Input is one compile. Every field but IdeaIDs and Actor is optional: the
// campaign defaults to the one the ideas share, the title to the first
// idea's, and the platform, post type, phase and date to the first idea plan
// that sets each. Each of several platforms gets its own post; a post type
// needs exactly one.
type Input struct {
	IdeaIDs     []string
	CampaignID  string
	Title       string
	PlatformIDs []string
	PostType    string
	PhaseID     string
	TargetDate  *models.CalendarDate
	// Actor is the user compiling; the post and its theses are theirs.
	Actor string
	// NoteOrigin is who wrote the theses: manual from the API, assistant
	// from the assistant.
	NoteOrigin models.PostNoteOrigin
}

// Result is the compiled posts, one per platform, and the ideas they came
// from, in order.
type Result struct {
	Posts []Compiled
	Ideas []models.Idea
}

// Compiled is one compiled post and its theses note.
type Compiled struct {
	Post *models.Post
	Note *models.PostNote
}

// Service compiles ideas into posts.
type Service struct {
	db        *bun.DB
	ideas     repository.IdeaRepository
	campaigns repository.CampaignRepository
	links     repository.PostIdeaRepository
	logs      repository.PostLogRepository
	hub       eventhub.Hub
	draft     Drafter
	now       func() time.Time
}

// New returns a Service. logs and hub may be nil (no audit entry, no event).
func New(db *bun.DB, ideas repository.IdeaRepository, campaigns repository.CampaignRepository, links repository.PostIdeaRepository, logs repository.PostLogRepository, hub eventhub.Hub) *Service {
	return &Service{db: db, ideas: ideas, campaigns: campaigns, links: links, logs: logs, hub: hub, now: time.Now}
}

// Compile makes a planned post from the ideas on each platform, in one
// transaction: theses from each idea, the ideas' source assets as its
// sources, and a link to each idea. The ideas are left as they are; one idea
// can feed several posts.
func (s *Service) Compile(ctx context.Context, in Input) (*Result, error) {
	if len([]rune(in.Title)) > maxTitle {
		return nil, ErrTitleTooLong
	}
	ideas, err := s.loadIdeas(ctx, in.IdeaIDs)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(ideas))
	for i, idea := range ideas {
		ids[i] = idea.ID
	}
	campaign, err := s.campaign(ctx, in.CampaignID, ideas)
	if err != nil {
		return nil, err
	}
	targets, err := compileTargets(in, ideas, campaign)
	if err != nil {
		return nil, err
	}
	res := &Result{Ideas: ideas}
	logs := make([]*models.PostLog, 0, len(targets))
	for _, t := range targets {
		post, err := s.build(in, t, ideas, campaign)
		if err != nil {
			return nil, err
		}
		note, err := thesesNote(post, ideas, in)
		if err != nil {
			return nil, err
		}
		logEntry, err := postaudit.NewLog(post.ID, models.PostLogEventPostCompiled, in.Actor,
			fmt.Sprintf("post compiled from %d idea(s)", len(ideas)), map[string]any{"idea_ids": ids})
		if err != nil {
			return nil, err
		}
		res.Posts = append(res.Posts, Compiled{Post: post, Note: note})
		logs = append(logs, logEntry)
	}

	if err := s.write(ctx, res.Posts, ids, logs); err != nil {
		return nil, err
	}
	for _, c := range res.Posts {
		postaudit.Publish(s.hub, c.Post.ID, "post.compiled", in.Actor, map[string]any{"ideaIds": ids})
	}
	return res, nil
}

// write saves the compiled posts in one transaction: each post, its theses,
// its links to the ideas and its audit entry.
func (s *Service) write(ctx context.Context, posts []Compiled, ideaIDs []string, logs []*models.PostLog) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for i, c := range posts {
			if _, err := tx.NewInsert().Model(c.Post).Exec(ctx); err != nil {
				return err
			}
			if _, err := tx.NewInsert().Model(c.Note).Exec(ctx); err != nil {
				return err
			}
			if err := s.links.Link(ctx, tx, c.Post.ID, ideaIDs); err != nil {
				return err
			}
			if s.logs == nil {
				continue
			}
			if err := s.logs.AppendTx(ctx, tx, logs[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if repository.IsConstraintViolation(err, repository.ConstraintPhaseMatchesCampaignType) {
		return ErrInvalidPhase
	}
	return err
}

// loadIdeas loads 1 to MaxIdeas distinct ideas, in the order given.
func (s *Service) loadIdeas(ctx context.Context, idList []string) ([]models.Idea, error) {
	ids := dedupe(idList, MaxIdeas)
	if len(ids) == 0 || len(ids) > MaxIdeas {
		return nil, ErrIdeaCount
	}
	ideas := make([]models.Idea, 0, len(ids))
	for _, id := range ids {
		idea, err := s.ideas.GetByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrIdeaNotFound
		}
		if err != nil {
			return nil, err
		}
		ideas = append(ideas, *idea)
	}
	return ideas, nil
}

// campaign resolves the post's campaign: the one asked for, else the one
// every idea in a campaign is in; an idea in no campaign goes along.
func (s *Service) campaign(ctx context.Context, id string, ideas []models.Idea) (*models.Campaign, error) {
	if id == "" {
		for _, idea := range ideas {
			switch {
			case idea.CampaignID == nil:
				continue
			case id == "":
				id = *idea.CampaignID
			case id != *idea.CampaignID:
				return nil, ErrMixedCampaigns
			}
		}
		if id == "" {
			return nil, ErrNoCampaign
		}
	}
	c, err := s.campaigns.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && c.DeletedAt != nil) {
		return nil, ErrCampaignGone
	}
	return c, err
}

// target is where one compiled post lands.
type target struct{ platformID, postType string }

// compileTargets is a post's platform and type for each platform asked for,
// checked against the campaign's; with none asked for, the one the first
// idea plan suggests, dropped when stale. A suggested post type goes with
// its suggested platform.
func compileTargets(in Input, ideas []models.Idea, campaign *models.Campaign) ([]target, error) {
	plan := firstPlan(ideas, campaign.ID)
	// One more platform than the campaign has is enough to find one that
	// isn't its own.
	platforms := dedupe(in.PlatformIDs, len(campaign.TargetPlatforms))
	if len(platforms) == 0 {
		p, t, err := fitTarget(campaign, deref(plan.PlatformID), cmp.Or(in.PostType, deref(plan.PostType)), false, in.PostType != "")
		return []target{{p, t}}, err
	}
	if in.PostType != "" && len(platforms) != 1 {
		return nil, ErrPostTypeOneFor
	}
	out := make([]target, len(platforms))
	for i, p := range platforms {
		postType := in.PostType
		if postType == "" && p == deref(plan.PlatformID) {
			postType = deref(plan.PostType)
		}
		p, t, err := fitTarget(campaign, p, postType, true, in.PostType != "")
		if err != nil {
			return nil, err
		}
		out[i] = target{p, t}
	}
	return out, nil
}

// build assembles one post for t: the request's fields, else the first idea
// plan that sets each.
func (s *Service) build(in Input, t target, ideas []models.Idea, campaign *models.Campaign) (*models.Post, error) {
	plan := firstPlan(ideas, campaign.ID)
	platformID, postType := t.platformID, t.postType
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	// At the microsecond Postgres keeps, so the post as returned matches
	// the post as read back.
	now := s.now().UTC().Truncate(time.Microsecond)
	sources := sourceAssets(ideas)
	post := &models.Post{
		ID:               id,
		CampaignID:       campaign.ID,
		PlatformID:       platformID,
		PlatformPostType: postType,
		Title:            cmp.Or(strings.TrimSpace(in.Title), ideas[0].Title),
		Content:          "",
		MediaURLs:        models.StringSlice{},
		Status:           models.PostStatusPlanned,
		CTAType:          models.CTATypeNone,
		UsedAssetIDs:     sources,
		GroundingMode:    models.GroundingFor(len(sources)),
		CreatedBy:        in.Actor,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if phase := cmp.Or(in.PhaseID, deref(plan.PhaseID)); phase != "" {
		post.CampaignTypePhaseID = &phase
	}
	date := in.TargetDate
	if date == nil {
		date = plan.TargetDate
	}
	if date != nil {
		loc, _ := settings.ResolveTimezone(campaign.Timezone)
		post.ScheduledAt, _, _ = scheduling.ComposeScheduledAt(date.String(), id, loc,
			campaign.PublishingTime, campaign.PublishingDays, campaign.SpreadMinutes, campaign.StartDate, campaign.EndDate)
	}
	return post, nil
}

// fitTarget checks a platform and post type against the campaign's targets,
// each by whether the caller named it: a named one that doesn't fit is
// refused, a suggested one is dropped. A post type left without a platform
// is refused when named and dropped when suggested.
func fitTarget(c *models.Campaign, platformID, postType string, askedPlatform, askedType bool) (string, string, error) {
	if platformID != "" {
		i := slices.IndexFunc(c.TargetPlatforms, func(p models.CampaignPlatform) bool { return p.ID == platformID })
		switch {
		case i < 0 && askedPlatform:
			return "", "", ErrNotTarget
		case i < 0:
			platformID = ""
		case postType != "" && len(c.TargetPlatforms[i].PostTypes) > 0 && !slices.Contains(c.TargetPlatforms[i].PostTypes, postType):
			if askedType {
				return "", "", ErrPostType
			}
			postType = ""
		}
	}
	if platformID == "" && postType != "" {
		if askedType {
			return "", "", ErrPostTypeAlone
		}
		postType = ""
	}
	return platformID, postType, nil
}

// thesesNote writes each idea as one thesis: its title, then its angle (or
// note).
func thesesNote(post *models.Post, ideas []models.Idea, in Input) (*models.PostNote, error) {
	var b strings.Builder
	for i, idea := range ideas {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- **%s**", strings.TrimSpace(idea.Title))
		body := cmp.Or(strings.TrimSpace(deref(idea.Angle)), strings.TrimSpace(idea.Note))
		for line := range strings.SplitSeq(body, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fmt.Fprintf(&b, "\n  - %s", strings.TrimLeft(line, "-* "))
			}
		}
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	return &models.PostNote{
		ID:        id,
		PostID:    post.ID,
		Type:      models.PostNoteTypeDraftThesis,
		Title:     "Draft thesis",
		Body:      b.String(),
		Origin:    cmp.Or(in.NoteOrigin, models.PostNoteOriginManual),
		CreatedBy: in.Actor,
		CreatedAt: post.CreatedAt,
		UpdatedAt: post.CreatedAt,
	}, nil
}

// sourceAssets is every asset the ideas drew on, in order: an analysis
// idea's source, then a planned idea's assets. They are the post's sources.
func sourceAssets(ideas []models.Idea) models.StringSlice {
	out := models.StringSlice{}
	add := func(id string) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, idea := range ideas {
		if idea.SourceAssetID != nil {
			add(*idea.SourceAssetID)
		}
		for _, id := range idea.AssetIDs {
			add(id)
		}
	}
	return out
}

// dedupe trims ids and drops blanks and repeats, keeping the first of each.
// It stops at limit+1 distinct ids: enough to tell the list is too long.
func dedupe(ids []string, limit int) []string {
	var out []string
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(out, id) {
			if out = append(out, id); len(out) > limit {
				break
			}
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
