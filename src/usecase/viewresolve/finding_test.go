package viewresolve

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
)

// fakeThreads is one campaign thread's messages, oldest first.
type fakeThreads struct {
	msgs []models.AssistantMessage
	got  string
}

func (f *fakeThreads) ListRecent(_ context.Context, kind models.AssistantScopeKind, id string, _ int) ([]models.AssistantMessage, error) {
	f.got = string(kind) + ":" + id
	return f.msgs, nil
}

const fromFinding = `{"version":1,"screen":"post","scope":{"kind":"post","id":"p1"},
	"from":{"kind":"finding","campaignId":"c1","key":"post_drift:k1"}}`

// The finding a post thread is opened from is the latest review's in the
// campaign's thread, whether a chat turn's results or a card's answer holds
// it; the view stores it.
func TestResolve_FromFinding(t *testing.T) {
	threads := &fakeThreads{msgs: []models.AssistantMessage{
		{Role: "model", Content: `{"results":{"repair":{"reviewCampaign":{"findings":[
			{"kind":"post","key":"post_drift:k1","postId":"p1","issue":"Old wording."}]}}}}`},
		{Role: "user", Content: `{"findings":[{"kind":"post","key":"post_drift:k1","postId":"p1","issue":"typed"}]}`},
		{Role: "model", Content: `{"result":{"status":"ok","result":{"findings":[
			{"kind":"brief","key":"post_drift:k1","issue":"a brief finding"},
			{"kind":"post","key":"post_drift:k1","postId":"p1","title":"Launch day","severity":"high",
			 "issue":"Too salesy.","quote":"Book now!!!","suggestion":"Invite, don't push."}]}}}`},
		{Role: "model", Content: `not json`},
	}}
	r := New(titles(), &fakeIdeas{}, &fakeAssets{}, &fakePosts{}).WithThreads(threads)
	v := parse(t, fromFinding)
	got, err := r.Resolve(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	want := viewcontext.Finding{Key: "post_drift:k1", PostID: "p1", Title: "Launch day", Severity: "high",
		Issue: "Too salesy.", Quote: "Book now!!!", Suggestion: "Invite, don't push."}
	if threads.got != "campaign:c1" || got.From == nil || *got.From != want {
		t.Fatalf("read %s, from = %+v", threads.got, got.From)
	}
	stored, err := v.Stored()
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := stored["from"].(map[string]any)["finding"].(map[string]any); f["quote"] != "Book now!!!" {
		t.Fatalf("stored from = %v", stored["from"])
	}
}

// A key no review has, or a finding about another post, is refused on
// from.key.
func TestResolve_FromFindingRefused(t *testing.T) {
	cases := map[string]string{
		"no such finding": `{"results":{"repair":{"reviewCampaign":{"findings":[{"kind":"post","key":"other","postId":"p1"}]}}}}`,
		"another post":    `{"results":{"repair":{"reviewCampaign":{"findings":[{"kind":"post","key":"post_drift:k1","postId":"p2"}]}}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			threads := &fakeThreads{msgs: []models.AssistantMessage{{Role: "model", Content: content}}}
			r := New(titles(), &fakeIdeas{}, &fakeAssets{}, &fakePosts{}).WithThreads(threads)
			_, err := r.Resolve(t.Context(), parse(t, fromFinding))
			if ie, ok := errors.AsType[*viewcontext.InvalidError](err); !ok || ie.Field != "from.key" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
