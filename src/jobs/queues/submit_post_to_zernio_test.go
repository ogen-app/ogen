package queues_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// fakePostRepo is the narrow surface the queue handlers actually
// touch. A real repository.PostRepository is overkill for unit tests
// of the queue; keeping a tiny in-memory shim makes failure modes
// obvious.
type fakePostRepo struct {
	mu    sync.Mutex
	posts map[string]*models.Post
}

func newFakePostRepo() *fakePostRepo { return &fakePostRepo{posts: map[string]*models.Post{}} }

func (r *fakePostRepo) put(p *models.Post) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *p
	r.posts[p.ID] = &cp
}
func (r *fakePostRepo) GetByID(_ context.Context, id string) (*models.Post, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.posts[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, errors.New("not found")
}
func (r *fakePostRepo) Update(_ context.Context, p *models.Post, _ ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *p
	r.posts[p.ID] = &cp
	return nil
}

// Stubs for the rest of the PostRepository surface — never called by
// the queues but required to satisfy the interface. We type-assert in
// the test setup so any forgotten method becomes a compile error.
func (r *fakePostRepo) List(context.Context) ([]models.Post, error) { return nil, nil }
func (r *fakePostRepo) ListByCampaign(context.Context, string) ([]models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) ListSummaryProjections(context.Context) ([]models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) ListAttachTargets(context.Context, string, int) ([]models.PostAttachTarget, error) {
	return nil, nil
}
func (r *fakePostRepo) ListCampaignPostTree(context.Context, int, int) ([]models.CampaignPostTree, error) {
	return nil, nil
}
func (r *fakePostRepo) UpdateScheduledAtBatch(context.Context, []*models.Post) error { return nil }
func (r *fakePostRepo) AddUsedAssetIDs(context.Context, string, []string) (*models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) RemoveUsedAssetID(context.Context, string, string) (*models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) ListWithPublisherPostID(context.Context) ([]models.Post, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []models.Post
	for _, p := range r.posts {
		if p.Publisher == models.PublisherZernio && p.PublisherPostID != "" {
			// Mirror the real repo's projection, including tenant_id — the
			// analytics refresh groups by it to sweep per-profile.
			out = append(out, models.Post{
				ID:              p.ID,
				TenantScoped:    models.TenantScoped{TenantID: p.TenantID},
				PublisherPostID: p.PublisherPostID,
				Title:           p.Title,
				PublishedAt:     p.PublishedAt,
				PlatformID:      p.PlatformID,
				Publisher:       p.Publisher,
			})
		}
	}
	return out, nil
}
func (r *fakePostRepo) CountPendingByAccount(context.Context, string) (int, error) { return 0, nil }
func (r *fakePostRepo) ListManualPublishDue(context.Context, time.Time, int) ([]models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) PublishedAtsBetween(context.Context, time.Time, time.Time, []string, string) ([]time.Time, error) {
	return nil, nil
}
func (r *fakePostRepo) ScopeKeysByID(context.Context, []string) (map[string]repository.PostScopeKey, error) {
	return nil, nil
}
func (r *fakePostRepo) ListPublishedSince(context.Context, time.Time) ([]models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) PublishedProjectionBetween(context.Context, time.Time, time.Time, string, int) ([]models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) CreatedProjectionBetween(context.Context, time.Time, time.Time, string, int) ([]models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) Create(context.Context, *models.Post) error        { return nil }
func (r *fakePostRepo) CreateBatch(context.Context, []*models.Post) error { return nil }
func (r *fakePostRepo) Delete(context.Context, string) (bool, error)      { return false, nil }
func (r *fakePostRepo) ListStuckScheduled(context.Context, time.Time, int) ([]models.Post, error) {
	return nil, nil
}
func (r *fakePostRepo) ListScheduledByPlatform(_ context.Context, platformID string) ([]models.Post, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []models.Post
	for _, p := range r.posts {
		if p.Status == models.PostStatusScheduled && p.PlatformID == platformID {
			out = append(out, *p)
		}
	}
	return out, nil
}
func (r *fakePostRepo) UpdateStatusAndReason(_ context.Context, id string, st models.PostStatus, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.posts[id]; ok {
		p.Status = st
		p.FailureReason = reason
	}
	return nil
}

// fakeLogRepo records every PostLog Append for assertions.
type fakeLogRepo struct {
	mu      sync.Mutex
	entries []*models.PostLog
}

func newFakeLogRepo() *fakeLogRepo { return &fakeLogRepo{} }
func (r *fakeLogRepo) Append(_ context.Context, entry *models.PostLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *entry
	r.entries = append(r.entries, &cp)
	return nil
}
func (r *fakeLogRepo) AppendTx(ctx context.Context, _ bun.IDB, e *models.PostLog) error {
	return r.Append(ctx, e)
}
func (r *fakeLogRepo) ListByPostID(context.Context, string, int) ([]models.PostLog, error) {
	return nil, nil
}
func (r *fakeLogRepo) ListFiltered(context.Context, repository.PostLogFilter) ([]models.PostLog, error) {
	return nil, nil
}
func (r *fakeLogRepo) DeleteOlderThan(context.Context, time.Time) (int64, error) { return 0, nil }
func (r *fakeLogRepo) TerminalTransitionsBetween(context.Context, time.Time, time.Time, string, int) ([]repository.TerminalTransition, error) {
	return nil, nil
}
func (r *fakeLogRepo) eventTypes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, string(e.EventType))
	}
	return out
}

// hasTransitionTo reports whether any recorded state_transition landed
// on the given status.
func (r *fakeLogRepo) hasTransitionTo(to models.PostStatus) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.EventType == models.PostLogEventStateTransition && e.ToStatus != nil && *e.ToStatus == to {
			return true
		}
	}
	return false
}

// fakeAccountRepo returns a fixed list per profile.
type fakeAccountRepo struct {
	accounts map[string][]models.SocialAccount
}

func (r *fakeAccountRepo) ListAll(context.Context, string) ([]models.SocialAccount, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListByIDs(context.Context, []string) ([]models.SocialAccount, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListActive(_ context.Context, profileID string) ([]models.SocialAccount, error) {
	return r.accounts[profileID], nil
}
func (r *fakeAccountRepo) ListActiveByPlatform(_ context.Context, profileID, platform string) ([]models.SocialAccount, error) {
	var out []models.SocialAccount
	for _, a := range r.accounts[profileID] {
		if a.Platform == platform && a.DeletedAt == nil {
			out = append(out, a)
		}
	}
	return out, nil
}
func (r *fakeAccountRepo) GetActive(_ context.Context, profileID, id string) (*models.SocialAccount, error) {
	for i := range r.accounts[profileID] {
		if a := r.accounts[profileID][i]; a.ID == id && a.DeletedAt == nil {
			return &a, nil
		}
	}
	return nil, sql.ErrNoRows
}
func (r *fakeAccountRepo) ApplyPlan(context.Context, []models.SocialAccount, []string, time.Time) error {
	return nil
}
func (r *fakeAccountRepo) SoftDelete(context.Context, string, time.Time) (bool, error) {
	return false, nil
}
func (r *fakeAccountRepo) UpdateHealth(context.Context, string, repository.SocialAccountHealth) error {
	return nil
}
func (r *fakeAccountRepo) ListActiveTenantProfiles(context.Context) ([]repository.TenantProfile, error) {
	seen := map[string]bool{}
	var out []repository.TenantProfile
	for profileID, accs := range r.accounts {
		for _, a := range accs {
			if a.DeletedAt != nil || profileID == "" {
				continue
			}
			key := a.TenantID + "\x00" + profileID
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, repository.TenantProfile{TenantID: a.TenantID, ProfileID: profileID})
		}
	}
	return out, nil
}

// stubZernio is the route-table HTTP server reused across queue tests.
type stubZernio struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
}

func newStubZernio() *stubZernio {
	s := &stubZernio{routes: map[string]http.HandlerFunc{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		h, ok := s.routes[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "stub: unhandled "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	return s
}

func (s *stubZernio) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func makeDeps(stub *stubZernio, accounts map[string][]models.SocialAccount) (queues.ZernioDeps, *fakePostRepo, *fakeLogRepo) {
	postRepo := newFakePostRepo()
	logRepo := newFakeLogRepo()
	deps := queues.ZernioDeps{
		PostRepo:          postRepo,
		PostLogRepo:       logRepo,
		SocialAccountRepo: &fakeAccountRepo{accounts: accounts},
		Client:            zernio.NewClient(zernio.StaticKey("k"), stub.URL, zernio.ClientOpts{Timeout: 5 * time.Second}),
		ProfileID:         func(context.Context) (string, error) { return "p_test", nil },
	}
	return deps, postRepo, logRepo
}

func seedScheduledPost(repo *fakePostRepo) *models.Post {
	now := time.Now().Add(-time.Minute).UTC()
	post := &models.Post{
		ID:          "post-1",
		PlatformID:  "AXqWG7U2qnpt", // LinkedIn Sqid
		Content:     "hello world",
		Status:      models.PostStatusScheduled,
		ScheduledAt: &now,
		Platform: &models.Platform{
			ID:   "AXqWG7U2qnpt",
			Name: "LinkedIn",
		},
	}
	repo.put(post)
	return post
}

func TestSubmitHappyPathPersistsZernioID(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-1", Status: zernio.JobStatusScheduled,
		}})
	})
	deps, postRepo, logRepo := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.PublisherPostID != "z-1" {
		t.Errorf("zernio_post_id: got %q want z-1", got.PublisherPostID)
	}
	events := logRepo.eventTypes()
	hasSubmit := false
	for _, e := range events {
		if e == string(models.PostLogEventZernioSubmit) {
			hasSubmit = true
		}
	}
	if !hasSubmit {
		t.Errorf("expected zernio_submit event, got %v", events)
	}
}

func TestSubmitTerminalRejectionMovesPostToFailed(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platforms required"})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should swallow terminal err: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Errorf("status: got %q want failed", got.Status)
	}
	if got.FailureReason == "" {
		t.Error("failure_reason should be set")
	}
}

func TestSubmitTransientErrorBubblesForRetry(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "later"})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID})
	if err == nil {
		t.Fatal("expected non-nil error so backlite retries")
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusScheduled {
		t.Errorf("status: got %q want scheduled (no transition on transient)", got.Status)
	}
}

func TestSubmitDedupeRecoveryAdoptsExistingJob(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate within 24h"})
	})
	stub.handle("GET", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"posts": []zernio.Job{{ID: "z-existing", Content: "hello world"}},
		})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.PublisherPostID != "z-existing" {
		t.Errorf("zernio_post_id: got %q want z-existing", got.PublisherPostID)
	}
}

func TestSubmitRetryOfVanishedPostCreatesFreshPost(t *testing.T) {
	// The stored Zernio post is gone (404 on retry): drop the stale id and
	// create a new post instead of failing terminally.
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts/z-stale/retry", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Post not found"})
	})
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-fresh", Status: zernio.JobStatusScheduled,
		}})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-stale"
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusScheduled {
		t.Errorf("status: got %q want scheduled (reason %q)", got.Status, got.FailureReason)
	}
	if got.PublisherPostID != "z-fresh" {
		t.Errorf("publisher_post_id: got %q want z-fresh", got.PublisherPostID)
	}
}

func TestSubmitRetryRejectionStaysTerminal(t *testing.T) {
	// Only a 404 means "gone"; any other 4xx on retry is still a rejection,
	// and no fresh post is created behind it.
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts/z-1/retry", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "post is not failed"})
	})
	var created atomic.Bool
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		created.Store(true)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{ID: "z-2"}})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should swallow terminal err: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Errorf("status: got %q want failed", got.Status)
	}
	if !strings.HasPrefix(got.FailureReason, "zernio_retry_rejected") {
		t.Errorf("failure_reason: got %q want zernio_retry_rejected…", got.FailureReason)
	}
	if created.Load() {
		t.Error("a rejected retry must not create a fresh Zernio post")
	}
}

func TestSubmitRetryTransientErrorBubbles(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts/z-1/retry", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "later"})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err == nil {
		t.Fatal("expected non-nil error so River retries")
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusScheduled || got.PublisherPostID != "z-1" {
		t.Errorf("got status=%q id=%q, want scheduled with z-1 kept", got.Status, got.PublisherPostID)
	}
}

func TestRescheduleAfterCancelCreatesFreshPost(t *testing.T) {
	// schedule → unschedule → schedule: the second submit must create a new
	// Zernio post rather than retry the one the cancel deleted.
	stub := newStubZernio()
	defer stub.Close()
	var submits, retries atomic.Int32
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		n := submits.Add(1)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{
			ID: fmt.Sprintf("z-%d", n), Status: zernio.JobStatusScheduled,
		}})
	})
	stub.handle("DELETE", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	stub.handle("POST", "/posts/z-1/retry", func(w http.ResponseWriter, r *http.Request) {
		retries.Add(1)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Post not found"})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)
	submit := &queues.SubmitPostProcessor{Deps: deps}
	cancel := &queues.CancelZernioJobProcessor{Deps: deps}

	if err := submit.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := cancel.Process(t.Context(), queues.CancelZernioJobTask{
		PostID: post.ID, Target: queues.CancelTargetReadyForPublish, Actor: "user-1",
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	got.Status = models.PostStatusScheduled // the user schedules it again
	postRepo.put(got)
	if err := submit.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("second submit: %v", err)
	}

	got, _ = postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusScheduled || got.PublisherPostID != "z-2" {
		t.Errorf("got status=%q id=%q (reason %q), want scheduled with z-2", got.Status, got.PublisherPostID, got.FailureReason)
	}
	if n := retries.Load(); n != 0 {
		t.Errorf("retry endpoint called %d times; a cancelled post must not be retried", n)
	}
}

func TestSubmitNoAccountConnectedFails(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {}, // no accounts
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should swallow terminal: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Errorf("status: got %q want failed", got.Status)
	}
}

// Two LinkedIn accounts, no explicit choice → terminal
// account_selection_required (never submitted, so the stub is never hit).
func TestSubmitMultipleAccountsRequiresSelection(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {
			{ID: "acc-1", Platform: "linkedin"},
			{ID: "acc-2", Platform: "linkedin"},
		},
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should swallow terminal: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Fatalf("status: got %q want failed", got.Status)
	}
	if !strings.HasPrefix(got.FailureReason, "account_selection_required") {
		t.Errorf("failure_reason: got %q want account_selection_required prefix", got.FailureReason)
	}
}

// An explicit account choice is sent to Zernio verbatim, even when
// the platform has several connected accounts.
func TestSubmitExplicitAccountSelectionIsUsed(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	var gotAccountID string
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		var body zernio.SubmitRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Platforms) == 1 {
			gotAccountID = body.Platforms[0].AccountID
		}
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{ID: "z-1", Status: zernio.JobStatusScheduled}})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {
			{ID: "acc-1", Platform: "linkedin"},
			{ID: "acc-2", Platform: "linkedin"},
		},
	})
	post := seedScheduledPost(postRepo)
	post.SocialAccountID = "acc-2"
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if gotAccountID != "acc-2" {
		t.Errorf("submitted accountId: got %q want acc-2", gotAccountID)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.PublisherPostID != "z-1" {
		t.Errorf("publisher_post_id: got %q want z-1", got.PublisherPostID)
	}
}

// An explicit choice on the wrong platform is a terminal mismatch.
func TestSubmitExplicitAccountPlatformMismatchFails(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {
			{ID: "acc-1", Platform: "linkedin"},
			{ID: "acc-x", Platform: "twitter"},
		},
	})
	post := seedScheduledPost(postRepo) // LinkedIn post
	post.SocialAccountID = "acc-x"      // ...pointed at a Twitter account
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should swallow terminal: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Fatalf("status: got %q want failed", got.Status)
	}
	if !strings.HasPrefix(got.FailureReason, "account_platform_mismatch") {
		t.Errorf("failure_reason: got %q want account_platform_mismatch prefix", got.FailureReason)
	}
}

// An explicit choice that is no longer connected is a terminal
// account_unavailable.
func TestSubmitExplicitAccountUnavailableFails(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)
	post.SocialAccountID = "ghost" // never connected
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should swallow terminal: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Fatalf("status: got %q want failed", got.Status)
	}
	if !strings.HasPrefix(got.FailureReason, "account_unavailable") {
		t.Errorf("failure_reason: got %q want account_unavailable prefix", got.FailureReason)
	}
}

// A link post's URL has no field of its own on Zernio, so it is sent as the
// last paragraph of the message, after the flattened body.
func TestSubmitLinkPostSendsURLInContent(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	var sent zernio.SubmitRequest
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-1", Status: zernio.JobStatusScheduled,
		}})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "facebook"}},
	})
	now := time.Now().Add(-time.Minute).UTC()
	post := &models.Post{
		ID:               "post-link",
		PlatformID:       "zBU1zqVICGfk", // Facebook Sqid
		PlatformPostType: models.PostTypeLinkPost,
		Content:          "**Big** news",
		CTAType:          models.CTATypeLink,
		CTAUrl:           "https://example.com/launch",
		Status:           models.PostStatusScheduled,
		ScheduledAt:      &now,
		Platform:         &models.Platform{ID: "zBU1zqVICGfk", Name: "Facebook"},
	}
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if want := "Big news\n\nhttps://example.com/launch"; sent.Content != want {
		t.Errorf("content: got %q want %q", sent.Content, want)
	}
	if len(sent.MediaItems) != 0 {
		t.Errorf("a link post sends no media, got %v", sent.MediaItems)
	}
}
