package update

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type fakePosts struct {
	repository.PostRepository
	updated []*models.Post
	omit    []string
}

func (f *fakePosts) Update(_ context.Context, p *models.Post, omit ...string) error {
	f.updated = append(f.updated, p)
	f.omit = omit
	return nil
}

func (f *fakePosts) GetByID(_ context.Context, id string) (*models.Post, error) {
	for _, p := range f.updated {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, errors.New("not found")
}

type fakeLogs struct {
	repository.PostLogRepository
	entries []*models.PostLog
}

func (f *fakeLogs) Append(_ context.Context, e *models.PostLog) error {
	f.entries = append(f.entries, e)
	return nil
}

type fakeCampaigns struct {
	repository.CampaignRepository
	belongs bool
	calls   int
}

func (f *fakeCampaigns) PhaseBelongsToCampaign(context.Context, string, string) (bool, error) {
	f.calls++
	return f.belongs, nil
}

func input(post *models.Post, to models.PostStatus) Input {
	return Input{
		Post:             post,
		Status:           to,
		Apply:            func(p *models.Post) { p.Status = to },
		CampaignID:       post.CampaignID,
		PlatformID:       "pl",
		PlatformPostType: "text",
		Actor:            "u1",
	}
}

func TestUpdateRejections(t *testing.T) {
	ctx := t.Context()
	phase, other := "ph1", "ph2"
	tests := []struct {
		name   string
		post   models.Post
		in     func(Input) Input
		belong bool
		check  func(t *testing.T, err error)
	}{
		{
			name: "invalid transition",
			post: models.Post{ID: "p", Status: models.PostStatusDraft},
			in:   func(in Input) Input { in.Status = models.PostStatusPublished; return in },
			check: func(t *testing.T, err error) {
				e, ok := errors.AsType[*TransitionError](err)
				if !ok || e.Error() != "invalid status transition from draft to published" {
					t.Fatalf("got %v", err)
				}
			},
		},
		{
			name: "content locked",
			post: models.Post{ID: "p", Status: models.PostStatusScheduled},
			in:   func(in Input) Input { in.MutatesLockedContent = true; return in },
			check: func(t *testing.T, err error) {
				if e, ok := errors.AsType[*ContentLockedError](err); !ok || e.Status != models.PostStatusScheduled {
					t.Fatalf("got %v", err)
				}
			},
		},
		{
			name: "platform required",
			post: models.Post{ID: "p", Status: models.PostStatusDraft},
			in: func(in Input) Input {
				in.Status = models.PostStatusReadyForPublish
				in.PlatformID = ""
				return in
			},
			check: func(t *testing.T, err error) {
				if e, ok := errors.AsType[*ValidationError](err); !ok || e.Msg != `platform_id is required when status is "ready_for_publish"` {
					t.Fatalf("got %v", err)
				}
			},
		},
		{
			name: "foreign phase",
			post: models.Post{ID: "p", Status: models.PostStatusDraft, CampaignTypePhaseID: &other},
			in:   func(in Input) Input { in.Status = models.PostStatusDraft; in.PhaseID = &phase; return in },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrInvalidPhase) {
					t.Fatalf("got %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts, logs := &fakePosts{}, &fakeLogs{}
			svc := &Service{Posts: posts, Logs: logs, Campaigns: &fakeCampaigns{belongs: tt.belong}}
			post := tt.post
			_, err := svc.Update(ctx, tt.in(input(&post, models.PostStatusDraft)))
			tt.check(t, err)
			if len(posts.updated) != 0 {
				t.Fatal("rejected edit must not persist")
			}
		})
	}
}

func TestUpdateTransitionBlockedIsLogged(t *testing.T) {
	logs := &fakeLogs{}
	svc := &Service{Posts: &fakePosts{}, Logs: logs}
	post := &models.Post{ID: "p", Status: models.PostStatusDraft}
	_, _ = svc.Update(t.Context(), input(post, models.PostStatusPublished))
	if len(logs.entries) != 1 || logs.entries[0].EventType != models.PostLogEventStateTransitionBlocked || logs.entries[0].Actor != "u1" {
		t.Fatalf("entries = %+v", logs.entries)
	}
}

func TestUpdatePersistsAndLogsTransition(t *testing.T) {
	posts, logs := &fakePosts{}, &fakeLogs{}
	camps := &fakeCampaigns{belongs: true}
	svc := &Service{Posts: posts, Logs: logs, Campaigns: camps}
	phase := "ph1"
	post := &models.Post{ID: "p", CampaignID: "c1", Status: models.PostStatusFailed, CampaignTypePhaseID: &phase}
	in := input(post, models.PostStatusReadyForPublish)
	in.PhaseID = &phase
	in.Omit = []string{"used_asset_ids"}

	res, err := svc.Update(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Post != post || post.Status != models.PostStatusReadyForPublish {
		t.Fatalf("result %+v", res)
	}
	if camps.calls != 0 {
		t.Fatal("an unchanged phase must not be re-checked")
	}
	if len(posts.omit) != 1 || posts.omit[0] != "used_asset_ids" {
		t.Fatalf("omit = %v", posts.omit)
	}
	var types []models.PostLogEventType
	for _, e := range logs.entries {
		types = append(types, e.EventType)
	}
	want := []models.PostLogEventType{models.PostLogEventStateTransition, models.PostLogEventUserRetry}
	if len(types) != 2 || types[0] != want[0] || types[1] != want[1] {
		t.Fatalf("log types = %v, want %v", types, want)
	}
}

func TestApplyThreadSegmentsClearsNonThread(t *testing.T) {
	p := &models.Post{PlatformPostType: "text", ThreadSegments: models.ThreadSegments{{Content: "x"}}}
	ApplyThreadSegments(p, 280)
	if p.ThreadSegments == nil || len(p.ThreadSegments) != 0 {
		t.Fatalf("segments = %#v", p.ThreadSegments)
	}
	if ThreadLimitOf(nil) != 0 {
		t.Fatal("nil platform must mean unknown limit")
	}
}

func TestHasValidationErrors(t *testing.T) {
	if HasValidationErrors(map[string][]platforms.ValidationError{"x": nil}) {
		t.Fatal("empty lists pass")
	}
	if !HasValidationErrors(map[string][]platforms.ValidationError{"x": {{Rule: "r"}}}) {
		t.Fatal("a listed error fails")
	}
}

func TestRequirePlatformIfNotDraft(t *testing.T) {
	if err := RequirePlatformIfNotDraft(models.PostStatusDraft, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := RequirePlatformIfNotDraft(models.PostStatusScheduled, "pl", ""); err == nil {
		t.Fatal("missing post type must fail")
	}
}
