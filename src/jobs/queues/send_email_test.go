package queues

import (
	"context"
	"database/sql"
	"expvar"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
)

// --- fakes ---------------------------------------------------------------

type fakeSender struct {
	sent []email.Message
	err  error
	id   string
}

func (f *fakeSender) Send(_ context.Context, msg email.Message) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.sent = append(f.sent, msg)
	return f.id, nil
}

type fakeUserRepo struct{ user *models.User }

func (f *fakeUserRepo) GetByIDWithTenant(_ context.Context, id string) (*models.User, error) {
	if f.user != nil && f.user.ID == id {
		return f.user, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeUserRepo) List(context.Context) ([]models.User, error)  { return nil, nil }
func (f *fakeUserRepo) CountInTenant(context.Context) (int64, error) { return 0, nil }
func (f *fakeUserRepo) Create(context.Context, *models.User) error   { return nil }
func (f *fakeUserRepo) GetByID(context.Context, string) (*models.User, error) {
	return nil, sql.ErrNoRows
}
func (f *fakeUserRepo) GetByEmail(context.Context, string) (*models.User, error) {
	return nil, sql.ErrNoRows
}
func (f *fakeUserRepo) GetByAccountID(context.Context, string) (*models.User, error) {
	return nil, sql.ErrNoRows
}
func (f *fakeUserRepo) GetMembership(context.Context, string, string) (*models.User, error) {
	return nil, sql.ErrNoRows
}
func (f *fakeUserRepo) Update(context.Context, *models.User) error   { return nil }
func (f *fakeUserRepo) Delete(context.Context, string) (bool, error) { return false, nil }
func (f *fakeUserRepo) CreateTx(context.Context, bun.IDB, *models.User) error {
	return nil
}
func (f *fakeUserRepo) SetRoleGuarded(context.Context, string, string, string) (*models.User, error) {
	return nil, nil
}
func (f *fakeUserRepo) RemoveMemberGuarded(context.Context, string, string) error { return nil }
func (f *fakeUserRepo) ListOwnersByTenant(context.Context, string) ([]models.User, error) {
	return nil, nil
}

type fakeTemplateRepo struct {
	m map[string]*models.EmailTemplate
}

func (f *fakeTemplateRepo) GetByKey(_ context.Context, key string) (*models.EmailTemplate, error) {
	if t, ok := f.m[key]; ok {
		return t, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeTemplateRepo) InsertIfAbsent(context.Context, *models.EmailTemplate) (bool, error) {
	return false, nil
}
func (f *fakeTemplateRepo) SyncVariables(context.Context, string, models.StringMap) error { return nil }
func (f *fakeTemplateRepo) List(context.Context) ([]models.EmailTemplate, error)          { return nil, nil }

// fakeSuppRepo mirrors the real SQL gate: marketing is blocked by any entry;
// transactional is blocked only by an `all`-scope entry.
type fakeSuppRepo struct {
	entries map[string]map[models.EmailSuppressionScope]bool
}

func newFakeSuppRepo() *fakeSuppRepo {
	return &fakeSuppRepo{entries: map[string]map[models.EmailSuppressionScope]bool{}}
}
func (f *fakeSuppRepo) add(email string, scope models.EmailSuppressionScope) {
	email = repository.NormalizeEmail(email)
	if f.entries[email] == nil {
		f.entries[email] = map[models.EmailSuppressionScope]bool{}
	}
	f.entries[email][scope] = true
}
func (f *fakeSuppRepo) Upsert(_ context.Context, s *models.EmailSuppression) error {
	f.add(s.Email, s.Scope)
	return nil
}
func (f *fakeSuppRepo) IsSuppressed(_ context.Context, addr string, kind models.EmailKind) (bool, error) {
	set := f.entries[repository.NormalizeEmail(addr)]
	if set == nil {
		return false, nil
	}
	if set[models.EmailSuppressionScopeAll] {
		return true, nil
	}
	if kind == models.EmailKindMarketing && set[models.EmailSuppressionScopeMarketing] {
		return true, nil
	}
	return false, nil
}
func (f *fakeSuppRepo) RemoveMarketing(_ context.Context, email string) error {
	if set := f.entries[repository.NormalizeEmail(email)]; set != nil {
		delete(set, models.EmailSuppressionScopeMarketing)
	}
	return nil
}

type fakeEmailLogRepo struct{ rows []*models.EmailLog }

func (f *fakeEmailLogRepo) Insert(_ context.Context, l *models.EmailLog) error {
	f.rows = append(f.rows, l)
	return nil
}
func (f *fakeEmailLogRepo) UpdateStatusByProviderMessageID(context.Context, string, models.EmailLogStatus) (bool, error) {
	return false, nil
}
func (f *fakeEmailLogRepo) DeleteOlderThan(context.Context, time.Time) (int64, error) { return 0, nil }
func (f *fakeEmailLogRepo) ExistsByIdempotencyKey(_ context.Context, key string) (bool, error) {
	for _, r := range f.rows {
		if r.IdempotencyKey != "" && r.IdempotencyKey == key {
			return true, nil
		}
	}
	return false, nil
}

// fakeActivityWriter satisfies activity.Writer so a real Recorder can be driven
// against an in-memory sink. Close drains onto the loop goroutine, so reads are
// only safe after the recorder is closed; the mutex keeps the race detector
// happy regardless.
type fakeActivityWriter struct {
	mu     sync.Mutex
	events []*models.ActivityEvent
}

func (w *fakeActivityWriter) Insert(_ context.Context, events []*models.ActivityEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, events...)
	return nil
}

func (w *fakeActivityWriter) all() []*models.ActivityEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*models.ActivityEvent(nil), w.events...)
}

// testActivityMetrics builds counters that are NOT registered on the global
// expvar registry, so many recorders can be built in one test binary without a
// duplicate-registration panic.
func testActivityMetrics() *activity.Metrics {
	return &activity.Metrics{
		EventsRecorded: new(expvar.Int),
		EventsDropped:  new(expvar.Int),
		WriteErrors:    new(expvar.Int),
	}
}

// --- harness -------------------------------------------------------------

func testUser() *models.User {
	return &models.User{ID: "u1", Name: "Ann", Email: "Ann@Example.com", Tenant: &models.Tenant{Name: "Acme"}}
}

func newProcessor(sender email.Sender, supp *fakeSuppRepo, logs *fakeEmailLogRepo, tmplHTML string) *SendEmailProcessor {
	return &SendEmailProcessor{Deps: EmailDeps{
		Sender:       sender,
		Users:        &fakeUserRepo{user: testUser()},
		Templates:    &fakeTemplateRepo{m: map[string]*models.EmailTemplate{"welcome": {Key: "welcome", Subject: "Hi [[ .Name ]]", HTML: tmplHTML, Text: "Hi [[ .Name ]]"}, "drip_day2": {Key: "drip_day2", Subject: "Drip", HTML: tmplHTML, Text: "t [[ .UnsubscribeURL ]]"}}},
		Suppressions: supp,
		Logs:         logs,
		From:         "Ogen <hello@ogen.test>",
		AppBaseURL:   "https://app.example",
		LinkBaseURL:  "https://app.example",
		LinkSecret:   func(context.Context) (string, error) { return "link-secret", nil },
	}}
}

func task(key string, kind models.EmailKind) SendEmailTask {
	return SendEmailTask{UserID: "u1", TenantID: "t1", TemplateKey: key, EmailKind: kind, IdempotencyKey: key + ":u1"}
}

// --- tests ---------------------------------------------------------------

func TestSendEmailHappyPathTransactional(t *testing.T) {
	sender := &fakeSender{id: "msg_1"}
	logs := &fakeEmailLogRepo{}
	p := newProcessor(sender, newFakeSuppRepo(), logs, "<p>Hi [[ .Name ]] at [[ .WorkspaceName ]]</p>")

	if err := p.Process(t.Context(), task("welcome", models.EmailKindTransactional)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}
	if sender.sent[0].To != "Ann@Example.com" {
		t.Fatalf("to: got %q", sender.sent[0].To)
	}
	if len(logs.rows) != 1 || logs.rows[0].Status != models.EmailLogSent || logs.rows[0].ProviderMessageID != "msg_1" {
		t.Fatalf("log row: %+v", logs.rows)
	}
}

func TestSendEmailDisabledWhenNoSender(t *testing.T) {
	logs := &fakeEmailLogRepo{}
	p := newProcessor(nil, newFakeSuppRepo(), logs, "<p>x</p>")

	if err := p.Process(t.Context(), task("welcome", models.EmailKindTransactional)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(logs.rows) != 1 || logs.rows[0].Status != models.EmailLogSkippedDisabled {
		t.Fatalf("want one skipped_disabled row, got %+v", logs.rows)
	}
}

func TestSendEmailMarketingSuppressed(t *testing.T) {
	sender := &fakeSender{id: "m"}
	supp := newFakeSuppRepo()
	supp.add("ann@example.com", models.EmailSuppressionScopeMarketing)
	logs := &fakeEmailLogRepo{}
	p := newProcessor(sender, supp, logs, "<p>x [[ .UnsubscribeURL ]]</p>")

	if err := p.Process(t.Context(), task("drip_day2", models.EmailKindMarketing)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatal("marketing to a marketing-suppressed address should not send")
	}
	if len(logs.rows) != 1 || logs.rows[0].Status != models.EmailLogSkippedSuppressed {
		t.Fatalf("want skipped_suppressed, got %+v", logs.rows)
	}
}

func TestSendEmailTransactionalNotBlockedByMarketingSuppression(t *testing.T) {
	sender := &fakeSender{id: "m"}
	supp := newFakeSuppRepo()
	supp.add("ann@example.com", models.EmailSuppressionScopeMarketing)
	p := newProcessor(sender, supp, &fakeEmailLogRepo{}, "<p>x</p>")

	if err := p.Process(t.Context(), task("welcome", models.EmailKindTransactional)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatal("transactional must still send to a marketing-unsubscribed address")
	}
}

func TestSendEmailAllScopeBlocksTransactional(t *testing.T) {
	sender := &fakeSender{id: "m"}
	supp := newFakeSuppRepo()
	supp.add("ann@example.com", models.EmailSuppressionScopeAll)
	logs := &fakeEmailLogRepo{}
	p := newProcessor(sender, supp, logs, "<p>x</p>")

	if err := p.Process(t.Context(), task("welcome", models.EmailKindTransactional)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatal("an all-scope suppression must block transactional mail too")
	}
	if logs.rows[0].Status != models.EmailLogSkippedSuppressed {
		t.Fatalf("want skipped_suppressed, got %s", logs.rows[0].Status)
	}
}

func TestSendEmailTransactionalNoUnsubscribeHeader(t *testing.T) {
	sender := &fakeSender{id: "t"}
	p := newProcessor(sender, newFakeSuppRepo(), &fakeEmailLogRepo{}, "<p>hi [[ .Name ]]</p>")

	if err := p.Process(t.Context(), task("welcome", models.EmailKindTransactional)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(sender.sent))
	}
	if _, ok := sender.sent[0].Headers["List-Unsubscribe"]; ok {
		t.Error("transactional mail must not carry a List-Unsubscribe header")
	}
}

// recordedFor drives p.Process against a real activity.Recorder and returns the
// drained events, so tests assert exactly what the analytics DB would receive.
func recordedFor(t *testing.T, p *SendEmailProcessor, tk SendEmailTask) []*models.ActivityEvent {
	t.Helper()
	w := &fakeActivityWriter{}
	rec := activity.NewRecorder(w, testActivityMetrics(), activity.Config{FlushEvery: time.Millisecond})
	p.Deps.ActivityRecorder = rec
	if err := p.Process(t.Context(), tk); err != nil {
		t.Fatalf("process: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := rec.Close(ctx); err != nil { // drain
		t.Fatalf("close recorder: %v", err)
	}
	return w.all()
}

// TestSendEmailRecordsActivity asserts every terminal send outcome writes one
// email-category event into the tenant activity log (CON-125), tenant/user
// scoped, mirroring the email_logs status.
func TestSendEmailRecordsActivity(t *testing.T) {
	assertOne := func(t *testing.T, events []*models.ActivityEvent, wantType, wantStatus string) *models.ActivityEvent {
		t.Helper()
		if len(events) != 1 {
			t.Fatalf("recorded %d activity events, want 1: %+v", len(events), events)
		}
		e := events[0]
		if e.Category != activity.CategoryEmail {
			t.Errorf("category = %q, want %q", e.Category, activity.CategoryEmail)
		}
		if e.Type != wantType {
			t.Errorf("type = %q, want %q", e.Type, wantType)
		}
		if e.Status != wantStatus {
			t.Errorf("status = %q, want %q", e.Status, wantStatus)
		}
		if e.TenantID != "t1" {
			t.Errorf("tenant = %q, want t1", e.TenantID)
		}
		if e.UserID != "u1" {
			t.Errorf("user = %q, want u1", e.UserID)
		}
		if e.Source != activity.SourceJob {
			t.Errorf("source = %q, want %q", e.Source, activity.SourceJob)
		}
		return e
	}

	t.Run("sent", func(t *testing.T) {
		p := newProcessor(&fakeSender{id: "msg_1"}, newFakeSuppRepo(), &fakeEmailLogRepo{}, "<p>Hi [[ .Name ]]</p>")
		e := assertOne(t, recordedFor(t, p, task("welcome", models.EmailKindTransactional)), "email_sent", string(models.EmailLogSent))
		if e.EntityType != "email" || e.EntityID != "welcome" {
			t.Errorf("entity = %s/%s, want email/welcome", e.EntityType, e.EntityID)
		}
		if e.Payload["provider_message_id"] != "msg_1" {
			t.Errorf("payload provider_message_id = %v, want msg_1", e.Payload["provider_message_id"])
		}
		if e.Payload["template"] != "welcome" || e.Payload["kind"] != string(models.EmailKindTransactional) {
			t.Errorf("payload = %+v", e.Payload)
		}
	})

	t.Run("skipped_disabled", func(t *testing.T) {
		p := newProcessor(nil, newFakeSuppRepo(), &fakeEmailLogRepo{}, "<p>x</p>")
		assertOne(t, recordedFor(t, p, task("welcome", models.EmailKindTransactional)), "email_skipped", string(models.EmailLogSkippedDisabled))
	})

	t.Run("skipped_suppressed", func(t *testing.T) {
		supp := newFakeSuppRepo()
		supp.add("ann@example.com", models.EmailSuppressionScopeMarketing)
		p := newProcessor(&fakeSender{id: "m"}, supp, &fakeEmailLogRepo{}, "<p>x [[ .UnsubscribeURL ]]</p>")
		assertOne(t, recordedFor(t, p, task("drip_day2", models.EmailKindMarketing)), "email_skipped", string(models.EmailLogSkippedSuppressed))
	})

	t.Run("failed", func(t *testing.T) {
		p := newProcessor(&fakeSender{id: "x"}, newFakeSuppRepo(), &fakeEmailLogRepo{}, "<p>x</p>")
		e := assertOne(t, recordedFor(t, p, task("does_not_exist", models.EmailKindTransactional)), "email_send_failed", string(models.EmailLogFailed))
		reason, _ := e.Payload["error"].(string)
		if !strings.Contains(reason, "template not found") {
			t.Errorf("payload error = %q, want it to mention 'template not found'", reason)
		}
	})
}

func TestSendEmailMarketingBuildsUnsubscribe(t *testing.T) {
	sender := &fakeSender{id: "m"}
	p := newProcessor(sender, newFakeSuppRepo(), &fakeEmailLogRepo{}, "<p>hi [[ .UnsubscribeURL ]]</p>")

	if err := p.Process(t.Context(), task("drip_day2", models.EmailKindMarketing)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(sender.sent))
	}
	msg := sender.sent[0]
	if _, ok := msg.Headers["List-Unsubscribe"]; !ok {
		t.Error("marketing message missing List-Unsubscribe header")
	}
	if msg.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("one-click header: got %q", msg.Headers["List-Unsubscribe-Post"])
	}
	if !strings.Contains(msg.HTML, "https://app.example/api/email/unsubscribe?token=") {
		t.Errorf("rendered html missing unsubscribe link: %q", msg.HTML)
	}
}
