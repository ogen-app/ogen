package templates

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func defaultByKey(t *testing.T, key string) *models.EmailTemplate {
	t.Helper()
	defs, err := Defaults()
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for i := range defs {
		if defs[i].Key == key {
			return &defs[i]
		}
	}
	t.Fatalf("default template %q not found", key)
	return nil
}

func TestRenderWelcomeInterpolates(t *testing.T) {
	r, err := Render(defaultByKey(t, KeyWelcome), Data{Name: "Ann", WorkspaceName: "Acme", AppURL: "https://app.example"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(r.Subject, "Ann") {
		t.Errorf("subject missing name: %q", r.Subject)
	}
	for _, want := range []string{"Ann", "Acme", "https://app.example"} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("html missing %q", want)
		}
	}
	if !strings.Contains(r.Text, "Ann") {
		t.Errorf("text missing name: %q", r.Text)
	}
}

func TestRenderDripHasUnsubscribe(t *testing.T) {
	const unsub = "https://app.example/api/email/unsubscribe?token=abc"
	r, err := Render(defaultByKey(t, KeyDripDay2), Data{Name: "Ann", WorkspaceName: "Acme", AppURL: "https://app.example", UnsubscribeURL: unsub})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(r.HTML, unsub) {
		t.Error("drip html missing unsubscribe URL")
	}
	if !strings.Contains(r.Text, unsub) {
		t.Error("drip text missing unsubscribe URL")
	}
}

// TestRenderCustomDelimsPassThroughBraces pins the Maizzle-compatibility
// decision: only [[ ]] interpolates; any {{ }} in the compiled HTML passes
// through untouched (CON-154 §11).
func TestRenderCustomDelimsPassThroughBraces(t *testing.T) {
	tmpl := &models.EmailTemplate{Key: "x", Subject: "s", HTML: "Hi [[ .Name ]] {{ keep-me }}", Text: "Hi [[ .Name ]]"}
	r, err := Render(tmpl, Data{Name: "Zed"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if r.HTML != "Hi Zed {{ keep-me }}" {
		t.Fatalf("html: got %q", r.HTML)
	}
}

// fakeTemplateRepo is an in-memory repository.EmailTemplateRepository.
type fakeTemplateRepo struct {
	m map[string]*models.EmailTemplate
}

func newFakeTemplateRepo() *fakeTemplateRepo {
	return &fakeTemplateRepo{m: map[string]*models.EmailTemplate{}}
}

func (f *fakeTemplateRepo) GetByKey(_ context.Context, key string) (*models.EmailTemplate, error) {
	if t, ok := f.m[key]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeTemplateRepo) InsertIfAbsent(_ context.Context, t *models.EmailTemplate) (bool, error) {
	if _, ok := f.m[t.Key]; ok {
		return false, nil
	}
	cp := *t
	f.m[t.Key] = &cp
	return true, nil
}

func (f *fakeTemplateRepo) SyncVariables(_ context.Context, key string, vars models.StringMap) error {
	if t, ok := f.m[key]; ok {
		t.Variables = vars
	}
	return nil
}

func (f *fakeTemplateRepo) List(context.Context) ([]models.EmailTemplate, error) {
	out := make([]models.EmailTemplate, 0, len(f.m))
	for _, t := range f.m {
		out = append(out, *t)
	}
	return out, nil
}

func TestSeedDefaultsIdempotent(t *testing.T) {
	repo := newFakeTemplateRepo()
	defs, _ := Defaults()

	n1, err := SeedDefaults(t.Context(), repo)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n1 != len(defs) {
		t.Fatalf("first seed: got %d, want %d", n1, len(defs))
	}

	// Variable docs are seeded: welcome (transactional) documents Name but not
	// UnsubscribeURL; a marketing drip documents UnsubscribeURL.
	if repo.m[KeyWelcome].Variables["Name"] == "" {
		t.Error("welcome missing Name variable doc")
	}
	if _, ok := repo.m[KeyWelcome].Variables["UnsubscribeURL"]; ok {
		t.Error("welcome (transactional) should not document UnsubscribeURL")
	}
	if repo.m[KeyDripDay2].Variables["UnsubscribeURL"] == "" {
		t.Error("drip missing UnsubscribeURL variable doc")
	}

	// Simulate an operator edit; the re-seed must not clobber it.
	edited := repo.m[KeyWelcome]
	edited.Subject = "Custom subject"

	n2, err := SeedDefaults(t.Context(), repo)
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("re-seed created %d rows; want 0 (all present)", n2)
	}
	if repo.m[KeyWelcome].Subject != "Custom subject" {
		t.Fatal("re-seed clobbered an operator edit")
	}
}
