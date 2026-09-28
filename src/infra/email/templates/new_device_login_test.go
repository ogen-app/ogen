package templates

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestRenderNewDeviceLogin(t *testing.T) {
	tmpl := defaultByKey(t, KeyNewDeviceLogin)
	if tmpl.Kind != models.EmailKindTransactional {
		t.Fatalf("kind = %q, want transactional so marketing unsubscribe never blocks it", tmpl.Kind)
	}
	data := Data{
		Name:        "Ann",
		LoginTime:   "28 Sep 2026, 14:03 UTC",
		DeviceLabel: "Chrome on macOS",
		IPAddress:   "203.0.113.42",
		Location:    "Kyiv, Ukraine",
		SecureURL:   "https://app.example/auth/secure-account?token=abc",
		MaskedEmail: "a***@acme.com",
	}

	r, err := Render(tmpl, data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if r.Subject != "New sign-in to your Ogen account" {
		t.Errorf("subject = %q", r.Subject)
	}
	for _, body := range []string{r.HTML, r.Text} {
		for _, want := range []string{"Ann", "28 Sep 2026, 14:03 UTC", "Chrome on macOS", "203.0.113.42", "Kyiv, Ukraine", "secure-account?token=abc", "a***@acme.com", "DB-IP"} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q", want)
			}
		}
	}

	data.Location = ""
	r, err = Render(tmpl, data)
	if err != nil {
		t.Fatalf("render without location: %v", err)
	}
	for _, body := range []string{r.HTML, r.Text} {
		if strings.Contains(body, "Approximate location") {
			t.Errorf("an unknown location must drop the row: %q", body)
		}
	}
}
