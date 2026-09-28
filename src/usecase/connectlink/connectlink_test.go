package connectlink

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
)

type fakeSettings struct {
	values map[string]string
	sets   int
}

func (f *fakeSettings) Get(_ context.Context, key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeSettings) Set(_ context.Context, key, value string) error {
	f.sets++
	f.values[key] = value
	return nil
}

func (f *fakeSettings) Delete(_ context.Context, key string) error {
	delete(f.values, key)
	return nil
}

func TestEnsureProfileExisting(t *testing.T) {
	s := &Service{Settings: &fakeSettings{values: map[string]string{zernio.SettingProfileID: "prof"}}}
	got, err := s.ensureProfile(t.Context())
	if err != nil || got != "prof" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestMarkConnectInitiatedOnce(t *testing.T) {
	st := &fakeSettings{values: map[string]string{}}
	s := &Service{Settings: st}
	for range 2 {
		if err := s.markConnectInitiated(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if st.sets != 1 || st.values[zernio.SettingConnectInitiatedAt] == "" {
		t.Fatalf("sets = %d, values = %v", st.sets, st.values)
	}
}

func TestOpenSessionRequiresTenant(t *testing.T) {
	s := &Service{}
	if _, _, err := s.openSession(t.Context(), "prof", "linkedin"); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("err = %v", err)
	}
}
