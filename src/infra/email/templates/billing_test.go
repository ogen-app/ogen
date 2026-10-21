package templates

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestRenderBillingNotices(t *testing.T) {
	data := Data{Name: "Jane", WorkspaceName: "Acme", AppURL: "https://app.example"}
	cases := map[string][]string{
		KeyBillingPaymentFailed:   {"Jane", "Acme", "https://app.example/plans", "retried"},
		KeyBillingReentryRequired: {"Jane", "Acme", "https://app.example/plans", "test period"},
	}
	for key, wants := range cases {
		tmpl := defaultByKey(t, key)
		if tmpl.Kind != models.EmailKindTransactional {
			t.Fatalf("%s kind = %q, want transactional", key, tmpl.Kind)
		}
		r, err := Render(tmpl, data)
		if err != nil {
			t.Fatalf("render %s: %v", key, err)
		}
		for _, body := range []string{r.HTML, r.Text} {
			for _, want := range wants {
				if !strings.Contains(body, want) {
					t.Errorf("%s body missing %q", key, want)
				}
			}
		}
	}
}
