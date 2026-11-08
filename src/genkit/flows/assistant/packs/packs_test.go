package packs

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

// turn is a pack tool's context in a thread, as user u1.
func turn(t *testing.T, scope viewcontext.Scope, onEvent assistant.OnEventFunc) (context.Context, func() []assistantkit.Touched) {
	t.Helper()
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	return assistant.NewTurnContext(ctx, scope, onEvent)
}

var (
	workspace = viewcontext.Scope{Kind: models.AssistantScopeWorkspace}
	campaign1 = viewcontext.Scope{Kind: models.AssistantScopeCampaign, ID: "c1"}
)

func refused(t *testing.T, err error) {
	t.Helper()
	if _, ok := errors.AsType[*assistantkit.Refusal](err); !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

type memActions struct {
	repository.AssistantActionRepository
	rows []*models.AssistantAction
}

func (m *memActions) Create(_ context.Context, a *models.AssistantAction) error {
	m.rows = append(m.rows, a)
	return nil
}

// memIdeaRepo keeps ideas for the ideas service.
type memIdeaRepo struct {
	repository.IdeaRepository
	byID map[string]*models.Idea
}

func (r *memIdeaRepo) GetByID(_ context.Context, id string) (*models.Idea, error) {
	if i, ok := r.byID[id]; ok {
		cp := *i
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (r *memIdeaRepo) Update(_ context.Context, i *models.Idea, _ ...string) (bool, error) {
	if _, ok := r.byID[i.ID]; !ok {
		return false, nil
	}
	cp := *i
	r.byID[i.ID] = &cp
	return true, nil
}

func (r *memIdeaRepo) DeleteMany(_ context.Context, ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		if _, ok := r.byID[id]; ok {
			delete(r.byID, id)
			out = append(out, id)
		}
	}
	return out, nil
}

func (r *memIdeaRepo) LiveCampaignExists(_ context.Context, id string) (bool, error) {
	return id == "c1", nil
}

func ideaDeps() (Deps, *memIdeaRepo, *memActions) {
	repo := &memIdeaRepo{byID: map[string]*models.Idea{
		"i1": {ID: "i1", Title: "Pricing myths"},
		"i2": {ID: "i2", Title: "Founder story"},
	}}
	actions := &memActions{}
	return Deps{Ideas: ideas.New(repo, nil), IdeaRepo: repo, Actions: actions, ProposalThreshold: 3}, repo, actions
}

func TestSetIdeaVerdict_RecordsUndo(t *testing.T) {
	d, repo, actions := ideaDeps()
	ctx, touched := turn(t, workspace, nil)
	_, err := setIdeaVerdict(ctx, d, SetIdeaVerdictInput{IdeaID: "i1", Verdict: "later", RemindAt: "2099-01-05"})
	if err == nil {
		t.Fatal("a reminder a year and more ahead is refused by the ideas service")
	}
	refused(t, err)

	out, err := setIdeaVerdict(ctx, d, SetIdeaVerdictInput{IdeaID: "i1", Verdict: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Idea.Verdict != "yes" || *repo.byID["i1"].Verdict != models.IdeaVerdictYes || out.UndoID == "" {
		t.Fatalf("out = %+v", out)
	}
	a := actions.rows[0]
	if a.Kind != models.AssistantActionIdeaVerdict || a.Before["verdict"] != nil || a.After["verdict"] != "yes" || a.ScopeKind != models.AssistantScopeWorkspace {
		t.Fatalf("action = %+v", a)
	}
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "idea", ID: "i1"}}) {
		t.Fatalf("touched = %v", touched())
	}
	_, err = setIdeaVerdict(ctx, d, SetIdeaVerdictInput{IdeaID: "i1", Verdict: "maybe"})
	refused(t, err)
	_, err = setIdeaVerdict(ctx, d, SetIdeaVerdictInput{IdeaID: "nope", Verdict: "yes"})
	refused(t, err)
}

func TestBulkIdeaChanges(t *testing.T) {
	d, repo, _ := ideaDeps()
	ctx, touched := turn(t, workspace, nil)
	out, err := setIdeaVerdicts(ctx, d, SetIdeaVerdictsInput{IdeaIDs: []string{"i1", "i2", "gone", "i1"}, Verdict: "no"})
	if err != nil || out.Done != 2 || !slices.Equal(out.Failed, []string{"gone"}) {
		t.Fatalf("verdicts = %+v, %v", out, err)
	}
	if out, err = moveIdeas(ctx, d, MoveIdeasInput{IdeaIDs: []string{"i1"}, CampaignID: "c1"}); err != nil || out.Done != 1 || *repo.byID["i1"].CampaignID != "c1" {
		t.Fatalf("move = %+v, %v", out, err)
	}
	_, err = moveIdeas(ctx, d, MoveIdeasInput{IdeaIDs: []string{"i1"}, CampaignID: "c9"})
	refused(t, err)
	if out, err = deleteIdeas(ctx, d, DeleteIdeasInput{IdeaIDs: []string{"i2", "gone"}}); err != nil || out.Done != 1 || len(out.Failed) != 1 {
		t.Fatalf("delete = %+v, %v", out, err)
	}
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "idea"}}) {
		t.Fatalf("touched = %v", touched())
	}
	refused(t, checkIDs(nil))
	if !overN(d, 4) || overN(d, 3) {
		t.Fatal("more than N waits for a yes")
	}
}

// failingIdeaRepo fails every write to the idea failID.
type failingIdeaRepo struct {
	*memIdeaRepo
	failID string
}

func (r failingIdeaRepo) Update(ctx context.Context, i *models.Idea, cols ...string) (bool, error) {
	if i.ID == r.failID {
		return false, errors.New("connection reset")
	}
	return r.memIdeaRepo.Update(ctx, i, cols...)
}

// A bulk change that fails part way reports and touches what it changed; one
// that changed nothing fails.
func TestBulkIdeaChanges_StoppedEarly(t *testing.T) {
	d, repo, _ := ideaDeps()
	d.Ideas = ideas.New(failingIdeaRepo{repo, "i2"}, nil)
	ctx, touched := turn(t, workspace, nil)

	out, err := moveIdeas(ctx, d, MoveIdeasInput{IdeaIDs: []string{"i1", "i2"}, CampaignID: "c1"})
	if err != nil || out.Done != 1 || out.Stopped != "stopped early: connection reset" || *repo.byID["i1"].CampaignID != "c1" {
		t.Fatalf("move = %+v, %v", out, err)
	}
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "idea"}}) {
		t.Fatalf("touched = %v", touched())
	}
	out, err = setIdeaVerdicts(ctx, d, SetIdeaVerdictsInput{IdeaIDs: []string{"i1", "i2"}, Verdict: "yes"})
	if err != nil || out.Done != 1 || out.Stopped == "" {
		t.Fatalf("verdicts = %+v, %v", out, err)
	}

	ctx, touched = turn(t, workspace, nil)
	if _, err := moveIdeas(ctx, d, MoveIdeasInput{IdeaIDs: []string{"i2", "i1"}}); err == nil {
		t.Fatal("a move that changed nothing must fail")
	}
	if len(touched()) != 0 {
		t.Fatalf("touched = %v; want nothing", touched())
	}
}

type memCampaigns struct {
	repository.CampaignRepository
	c *models.Campaign
}

func (r *memCampaigns) GetByID(_ context.Context, id string) (*models.Campaign, error) {
	if r.c != nil && r.c.ID == id {
		cp := *r.c
		cp.AssetIDs = slices.Clone(r.c.AssetIDs)
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (r *memCampaigns) Update(_ context.Context, c *models.Campaign, _ ...string) error {
	cp := *c
	r.c = &cp
	return nil
}

// ChangeAssetIDs keeps UseAssets in step with the set, like the repository's
// membership write.
func (r *memCampaigns) ChangeAssetIDs(ctx context.Context, id string, add, remove []string) (before, after *models.Campaign, err error) {
	if before, err = r.GetByID(ctx, id); err != nil {
		return nil, nil, err
	}
	r.c.AssetIDs = slices.DeleteFunc(r.c.AssetIDs, func(a string) bool { return slices.Contains(remove, a) })
	for _, a := range add {
		if !slices.Contains(r.c.AssetIDs, a) {
			r.c.AssetIDs = append(r.c.AssetIDs, a)
		}
	}
	r.c.UseAssets = len(r.c.AssetIDs) > 0
	after, err = r.GetByID(ctx, id)
	return before, after, err
}

type memAssets struct {
	repository.AssetRepository
	all []models.Asset
}

func (r memAssets) ListMeta(_ context.Context, ids []string) ([]models.Asset, error) {
	var out []models.Asset
	for _, a := range r.all {
		if ids == nil || slices.Contains(ids, a.ID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func TestAttachAssets(t *testing.T) {
	camps := &memCampaigns{c: &models.Campaign{ID: "c1", AssetIDs: models.StringSlice{"a1"}}}
	actions := &memActions{}
	d := Deps{Campaigns: camps, Assets: memAssets{all: []models.Asset{{ID: "a1"}, {ID: "a2", Status: "ready"}}}, Actions: actions}
	ctx, touched := turn(t, campaign1, nil)

	out, err := attachAssets(ctx, d, AttachAssetsInput{AssetIDs: []string{"a2", "a9"}})
	if err != nil || !slices.Equal(out.AssetIDs, []string{"a1", "a2"}) || out.UndoID == "" {
		t.Fatalf("attach = %+v, %v; want the existing asset added, the unknown one skipped", out, err)
	}
	if a := actions.rows[0]; a.Kind != models.AssistantActionCampaignAssets || len(a.Before["assetIds"].([]any)) != 1 {
		t.Fatalf("action = %+v", a)
	}
	if !camps.c.UseAssets {
		t.Fatal("attach left UseAssets off; planning would ignore the sources")
	}
	if out, err = attachAssets(ctx, d, AttachAssetsInput{AssetIDs: []string{"a1"}, Detach: true}); err != nil || !slices.Equal(out.AssetIDs, []string{"a2"}) {
		t.Fatalf("detach = %+v, %v", out, err)
	}
	if out, err = attachAssets(ctx, d, AttachAssetsInput{AssetIDs: []string{"a2"}, Detach: true}); err != nil || len(out.AssetIDs) != 0 || camps.c.UseAssets {
		t.Fatalf("detach the last = %+v, %v, UseAssets %v; want none and UseAssets off", out, err, camps.c.UseAssets)
	}
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "campaign", ID: "c1"}}) {
		t.Fatalf("touched = %v", touched())
	}

	ctx, _ = turn(t, workspace, nil)
	_, err = attachAssets(ctx, d, AttachAssetsInput{AssetIDs: []string{"a2"}})
	refused(t, err)
}

type sourceURLAssets struct {
	repository.AssetRepository
	looked []string
}

func (r *sourceURLAssets) GetBySourceURL(_ context.Context, u string) (*models.Asset, error) {
	r.looked = append(r.looked, u)
	return nil, errors.New("stop here")
}

type keyGate struct{}

func (keyGate) HasKey(context.Context) bool { return true }

type noJobs struct{}

func (noJobs) EnqueueProcessURLTx(context.Context, *sql.Tx, string, string, string, bool) error {
	return nil
}

func TestImportPage_NormalizesURL(t *testing.T) {
	assets := &sourceURLAssets{}
	d := Deps{URLImport: &urlimport.Service{DB: &bun.DB{}, Assets: assets, Jobs: noJobs{}, Gate: keyGate{}}}
	ctx, _ := turn(t, workspace, nil)

	_, err := importPage(ctx, d, ImportPageInput{URL: " HTTPS://93.184.216.34/Pricing/#plans "})
	refused(t, err)
	if !slices.Equal(assets.looked, []string{"https://93.184.216.34/Pricing"}) {
		t.Fatalf("looked up %q; want the canonical URL", assets.looked)
	}

	_, err = importPage(ctx, d, ImportPageInput{URL: "http://127.0.0.1/admin"})
	refused(t, err)
	if len(assets.looked) != 1 {
		t.Fatal("a private host reached the import")
	}
}

func TestAskAssets_WithoutSearch(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	out, err := askAssets(ctx, Deps{Assets: memAssets{}}, AskAssetsInput{Question: "pricing?"})
	if err != nil || out.Note == "" || out.Excerpts == nil {
		t.Fatalf("out = %+v, %v; want a note, not an error", out, err)
	}
	_, err = askAssets(ctx, Deps{}, AskAssetsInput{Question: " "})
	refused(t, err)
}

func TestSearchScope(t *testing.T) {
	d := Deps{
		Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1", AssetIDs: models.StringSlice{"a1", "a2"}}},
		Assets: memAssets{all: []models.Asset{
			{ID: "a1", Status: "ready"}, {ID: "a2", Status: "failed"}, {ID: "a3", Status: "partial"},
		}},
	}
	ctx, _ := turn(t, campaign1, nil)
	if ids, err := searchScope(ctx, d, nil); err != nil || !slices.Equal(ids, []string{"a1"}) {
		t.Fatalf("campaign scope = %v, %v; want its ready assets", ids, err)
	}
	if ids, _ := searchScope(ctx, d, []string{"a3"}); !slices.Equal(ids, []string{"a3"}) {
		t.Fatalf("asked = %v", ids)
	}
	ctx, _ = turn(t, workspace, nil)
	if ids, _ := searchScope(ctx, d, nil); !slices.Equal(ids, []string{"a1", "a3"}) {
		t.Fatalf("workspace = %v", ids)
	}
}

func TestParseVerdict(t *testing.T) {
	for in, want := range map[string]string{"yes": "yes", "Later": "later", "no": "no", "": "", "none": ""} {
		v, _, err := parseVerdict(in, "")
		if err != nil || (v == nil) != (want == "") || (v != nil && string(*v) != want) {
			t.Errorf("%q = %v, %v", in, v, err)
		}
	}
	if _, at, err := parseVerdict("later", "2026-12-01"); err != nil || at.Format("2006-01-02") != "2026-12-01" {
		t.Errorf("date remindAt = %v, %v", at, err)
	}
	if _, _, err := parseVerdict("later", "soon"); err == nil {
		t.Error("a bad remindAt passed")
	}
}

func TestAll(t *testing.T) {
	if n := len(All(Deps{})); n != 0 {
		t.Fatalf("no services = %d packs", n)
	}
	d, _, _ := ideaDeps()
	d.Campaign = &assistant.Pack{Name: "campaign"}
	if all := All(d); len(all) != 1 || all[0].Name != "campaign" {
		t.Fatalf("campaign only = %d packs (ideas needs compose)", len(all))
	}
}
