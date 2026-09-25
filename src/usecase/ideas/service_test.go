package ideas

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// stubRepo keeps one idea in memory and records the columns of the last Update.
type stubRepo struct {
	repository.IdeaRepository
	idea          *models.Idea
	columns       []string
	liveCampaigns map[string]bool
}

func (s *stubRepo) GetByID(_ context.Context, id string) (*models.Idea, error) {
	if s.idea == nil || s.idea.ID != id {
		return nil, errNoRows
	}
	cp := *s.idea
	return &cp, nil
}

func (s *stubRepo) Create(_ context.Context, i *models.Idea) error {
	s.idea = i
	return nil
}

func (s *stubRepo) Update(_ context.Context, i *models.Idea, columns ...string) (bool, error) {
	s.columns = columns
	s.idea = i
	return true, nil
}

func (s *stubRepo) LiveCampaignExists(_ context.Context, id string) (bool, error) {
	return s.liveCampaigns[id], nil
}

type stubUsers struct {
	repository.UserRepository
	user *models.User
}

func (s *stubUsers) GetByID(context.Context, string) (*models.User, error) { return s.user, nil }

var errNoRows = sql.ErrNoRows

var fixedNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func newTestService(repo *stubRepo, user *models.User) *Service {
	svc := New(repo, &stubUsers{user: user})
	svc.now = func() time.Time { return fixedNow }
	return svc
}

func ptr[T any](v T) *T { return &v }

func TestCreate_StampsAuthorAndTrims(t *testing.T) {
	repo := &stubRepo{}
	svc := newTestService(repo, &models.User{ID: "u1", Name: "Ada", Email: "ada@example.com"})

	idea, err := svc.Create(t.Context(), CreateInput{Title: "  An idea  ", Note: "  n  ", UserID: "u1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if idea.Title != "An idea" || idea.Note != "n" {
		t.Errorf("not trimmed: %q / %q", idea.Title, idea.Note)
	}
	if idea.CreatedBy == nil || *idea.CreatedBy != "u1" || idea.CreatedByName != "Ada" {
		t.Errorf("author = %v / %q", idea.CreatedBy, idea.CreatedByName)
	}
	if idea.Verdict != nil || idea.DecidedAt != nil || idea.CampaignID != nil {
		t.Errorf("new idea should be undecided and workspace-wide: %+v", idea)
	}
}

func TestCreate_AuthorNameFallsBackToEmail(t *testing.T) {
	svc := newTestService(&stubRepo{}, &models.User{ID: "u1", Name: " ", Email: "ada@example.com"})
	idea, err := svc.Create(t.Context(), CreateInput{Title: "x", UserID: "u1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if idea.CreatedByName != "ada@example.com" {
		t.Errorf("created_by_name = %q", idea.CreatedByName)
	}
}

func TestCreate_Validation(t *testing.T) {
	svc := newTestService(&stubRepo{}, &models.User{ID: "u1", Name: "Ada"})
	long := make([]rune, MaxTitleLen+1)
	for i := range long {
		long[i] = 'a'
	}
	cases := map[string]struct {
		in   CreateInput
		want error
	}{
		"blank title":      {CreateInput{Title: "   "}, ErrEmptyTitle},
		"long title":       {CreateInput{Title: string(long)}, ErrTitleTooLong},
		"unknown campaign": {CreateInput{Title: "x", CampaignID: ptr("nope")}, ErrCampaignNotFound},
	}
	for name, tc := range cases {
		if _, err := svc.Create(t.Context(), tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
		if !IsValidation(tc.want) {
			t.Errorf("%s: %v should be a validation error", name, tc.want)
		}
	}
}

func TestUpdate_WritesOnlyPresentFields(t *testing.T) {
	repo := &stubRepo{
		idea:          &models.Idea{ID: "i1", Title: "old", Note: "keep", Verdict: ptr(models.IdeaVerdictYes)},
		liveCampaigns: map[string]bool{"c1": true},
	}
	svc := newTestService(repo, nil)

	idea, fields, err := svc.Update(t.Context(), "i1", UpdateInput{Title: ptr(" new "), SetCampaign: true, CampaignID: ptr("c1")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if idea.Title != "new" || idea.Note != "keep" || *idea.CampaignID != "c1" {
		t.Errorf("patch not applied: %+v", idea)
	}
	if *idea.Verdict != models.IdeaVerdictYes {
		t.Error("attaching must keep the verdict")
	}
	if len(fields) != 2 || fields[0] != "title" || fields[1] != "campaign_id" {
		t.Errorf("fields = %v", fields)
	}

	// Detach.
	idea, _, err = svc.Update(t.Context(), "i1", UpdateInput{SetCampaign: true})
	if err != nil || idea.CampaignID != nil {
		t.Errorf("detach: %v / %v", err, idea.CampaignID)
	}

	if _, _, err := svc.Update(t.Context(), "missing", UpdateInput{Note: ptr("")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing idea: got %v", err)
	}
}

func TestSetVerdict_Invariants(t *testing.T) {
	repo := &stubRepo{idea: &models.Idea{ID: "i1", Title: "t"}}
	svc := newTestService(repo, nil)
	week := fixedNow.Add(7 * 24 * time.Hour)

	// later + remind_at stamps the decision.
	idea, prev, err := svc.SetVerdict(t.Context(), "i1", ptr(models.IdeaVerdictLater), &week, "u2")
	if err != nil {
		t.Fatalf("later: %v", err)
	}
	if prev != nil || *idea.Verdict != models.IdeaVerdictLater || !idea.RemindAt.Equal(week) ||
		!idea.DecidedAt.Equal(fixedNow) || *idea.DecidedBy != "u2" {
		t.Errorf("later not stamped: %+v", idea)
	}

	// yes clears remind_at.
	idea, prev, err = svc.SetVerdict(t.Context(), "i1", ptr(models.IdeaVerdictYes), nil, "u3")
	if err != nil || *prev != models.IdeaVerdictLater || idea.RemindAt != nil || *idea.DecidedBy != "u3" {
		t.Errorf("yes: err=%v prev=%v idea=%+v", err, prev, idea)
	}

	// null returns to the inbox and clears every decision field.
	idea, _, err = svc.SetVerdict(t.Context(), "i1", nil, nil, "u3")
	if err != nil || idea.Verdict != nil || idea.RemindAt != nil || idea.DecidedAt != nil || idea.DecidedBy != nil {
		t.Errorf("inbox: err=%v idea=%+v", err, idea)
	}
	if len(repo.columns) != 4 {
		t.Errorf("verdict write should touch the four decision columns, got %v", repo.columns)
	}
}

func TestSetVerdict_Validation(t *testing.T) {
	svc := newTestService(&stubRepo{idea: &models.Idea{ID: "i1", Title: "t"}}, nil)
	past := fixedNow.Add(-2 * time.Minute)
	skew := fixedNow.Add(-30 * time.Second)
	far := fixedNow.Add(400 * 24 * time.Hour)
	soon := fixedNow.Add(time.Hour)

	cases := map[string]struct {
		verdict  *models.IdeaVerdict
		remindAt *time.Time
		want     error
	}{
		"bogus verdict":      {ptr(models.IdeaVerdict("maybe")), nil, ErrInvalidVerdict},
		"later without date": {ptr(models.IdeaVerdictLater), nil, ErrRemindAtRequired},
		"yes with date":      {ptr(models.IdeaVerdictYes), &soon, ErrRemindAtNotAllowed},
		"null with date":     {nil, &soon, ErrRemindAtNotAllowed},
		"later in the past":  {ptr(models.IdeaVerdictLater), &past, ErrRemindAtOutOfRange},
		"later too far":      {ptr(models.IdeaVerdictLater), &far, ErrRemindAtOutOfRange},
		"later within skew":  {ptr(models.IdeaVerdictLater), &skew, nil},
	}
	for name, tc := range cases {
		_, _, err := svc.SetVerdict(t.Context(), "i1", tc.verdict, tc.remindAt, "u1")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}
