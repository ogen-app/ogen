package server

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	secretsv1 "github.com/ogen-app/ogen/gen/secrets/v1"
	"github.com/ogen-app/ogen/src/infra/secrets"
)

// fakeStore is a minimal in-memory secrets.Store for exercising the gRPC
// service's mapping without the envelope crypto. Only the methods the service
// calls (List/Set/Delete) are meaningful; the rest satisfy the interface.
type fakeStore struct {
	rows      map[string]secrets.Metadata
	setErr    error
	deleteErr error
	listErr   error
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]secrets.Metadata{}} }

func (f *fakeStore) List(context.Context) ([]secrets.Metadata, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]secrets.Metadata, 0, len(f.rows))
	for _, m := range f.rows {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeStore) Set(_ context.Context, name, _ string) (secrets.Metadata, bool, error) {
	if f.setErr != nil {
		return secrets.Metadata{}, false, f.setErr
	}
	_, existed := f.rows[name]
	m := secrets.Metadata{Name: name, UpdatedAt: time.Unix(100, 0), Algorithm: "AES-256-GCM", KEKVersion: 1, Decryptable: true}
	f.rows[name] = m
	return m, !existed, nil
}

func (f *fakeStore) Delete(context.Context, string) error { return f.deleteErr }

func (f *fakeStore) Get(context.Context, string) (string, error) { return "", nil }
func (f *fakeStore) GetMetadata(context.Context, string) (secrets.Metadata, error) {
	return secrets.Metadata{}, nil
}
func (f *fakeStore) Subscribe(string, func()) func() { return func() {} }

func TestList_MergesAllowlist(t *testing.T) {
	f := newFakeStore()
	f.rows[secrets.NameAnthropicAPIKey] = secrets.Metadata{Name: secrets.NameAnthropicAPIKey, UpdatedAt: time.Unix(10, 0)}
	svc := newSecretsService(f)

	resp, err := svc.List(t.Context(), &secretsv1.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := len(resp.GetSecrets()), len(secrets.AllowedNames); got != want {
		t.Fatalf("got %d entries, want one per allowlist name (%d)", got, want)
	}
	// Entries follow allowlist order; the anthropic slot is set, others are not.
	for i, name := range secrets.AllowedNames {
		e := resp.GetSecrets()[i]
		if e.GetName() != name {
			t.Errorf("entry %d name = %q, want %q", i, e.GetName(), name)
		}
		wantSet := name == secrets.NameAnthropicAPIKey
		if e.GetSet() != wantSet {
			t.Errorf("entry %q set = %v, want %v", name, e.GetSet(), wantSet)
		}
	}
}

func TestSet_Created(t *testing.T) {
	f := newFakeStore()
	svc := newSecretsService(f)

	// First Set of an absent name reports created=true.
	resp, err := svc.Set(t.Context(), &secretsv1.SetRequest{Name: secrets.NameGeminiAPIKey, Value: "k"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !resp.GetCreated() {
		t.Errorf("created = false, want true")
	}
	// A second Set of the same name is a rotation, not a create.
	resp2, _ := svc.Set(t.Context(), &secretsv1.SetRequest{Name: secrets.NameGeminiAPIKey, Value: "k2"})
	if resp2.GetCreated() {
		t.Errorf("created = true on rotation, want false")
	}
	if !resp.GetSecret().GetSet() {
		t.Errorf("returned metadata set = false, want true")
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		storeErr error
		want     codes.Code
	}{
		{"unknown name", secrets.ErrUnknownName, codes.InvalidArgument},
		{"invalid value", secrets.ErrInvalidValue, codes.InvalidArgument},
		{"not found", secrets.ErrNotFound, codes.NotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeStore()
			f.setErr = tc.storeErr
			svc := newSecretsService(f)
			_, err := svc.Set(t.Context(), &secretsv1.SetRequest{Name: "x"})
			if got := status.Code(err); got != tc.want {
				t.Errorf("code = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDelete_NotFound(t *testing.T) {
	f := newFakeStore()
	f.deleteErr = secrets.ErrNotFound
	svc := newSecretsService(f)
	_, err := svc.Delete(t.Context(), &secretsv1.DeleteRequest{Name: secrets.NameZernioAPIKey})
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("code = %v, want NotFound", got)
	}
}

func TestTokenAuthInterceptor(t *testing.T) {
	const token = "s3cr3t"
	interceptor := tokenAuthInterceptor(token)
	ok := grpc.UnaryHandler(func(context.Context, any) (any, error) { return "ok", nil })

	cases := []struct {
		name string
		ctx  context.Context
		want codes.Code // OK == valid
	}{
		{"valid", metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer "+token)), codes.OK},
		{"wrong token", metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer nope")), codes.Unauthenticated},
		{"no header", metadata.NewIncomingContext(t.Context(), metadata.MD{}), codes.Unauthenticated},
		{"no metadata", t.Context(), codes.Unauthenticated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := interceptor(tc.ctx, nil, &grpc.UnaryServerInfo{}, ok)
			if got := status.Code(err); got != tc.want {
				t.Errorf("code = %v, want %v", got, tc.want)
			}
		})
	}
}
