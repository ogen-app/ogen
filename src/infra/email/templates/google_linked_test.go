package templates

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestRenderGoogleLinked(t *testing.T) {
	tmpl := defaultByKey(t, KeyGoogleLinked)
	if tmpl.Kind != models.EmailKindTransactional {
		t.Fatalf("kind = %q, want transactional", tmpl.Kind)
	}
	data := Data{Name: "Jane", AppURL: "https://app.example", GoogleEmail: "jane@gmail.com", PasswordCleared: true}

	r, err := Render(tmpl, data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, body := range []string{r.HTML, r.Text} {
		for _, want := range []string{"Jane", "jane@gmail.com", "password it had before was removed", "https://app.example/auth/forgot"} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q", want)
			}
		}
	}

	data.PasswordCleared = false
	r, err = Render(tmpl, data)
	if err != nil {
		t.Fatalf("render without cleared password: %v", err)
	}
	for _, body := range []string{r.HTML, r.Text} {
		if strings.Contains(body, "was removed") {
			t.Errorf("an account that kept its password must not be told it was removed: %q", body)
		}
	}
}
