package server

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	secretsv1 "github.com/ogen-app/ogen/gen/secrets/v1"
	"github.com/ogen-app/ogen/src/infra/secrets"
)

// TestServerRoundTrip exercises the full gRPC path Harbor uses — a live listener
// + the token interceptor + a generated client sending a bearer token in
// metadata — so the wire contract, auth handshake, and allowlist merge are all
// covered together (the piece unit tests can't reach).
func TestServerRoundTrip(t *testing.T) {
	const token = "round-trip-token"
	store := newFakeStore()

	srv, err := New(token, store, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	newClient := func(tok string) secretsv1.SecretsServiceClient {
		conn, err := grpc.NewClient(
			lis.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
				return invoker(ctx, method, req, reply, cc, opts...)
			}),
		)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return secretsv1.NewSecretsServiceClient(conn)
	}

	ctx := t.Context()
	cli := newClient(token)

	// List starts with every allowlist slot present and unset.
	resp, err := cli.List(ctx, &secretsv1.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetSecrets()) != len(secrets.AllowedNames) {
		t.Fatalf("List returned %d slots, want %d", len(resp.GetSecrets()), len(secrets.AllowedNames))
	}
	for _, s := range resp.GetSecrets() {
		if s.GetSet() {
			t.Errorf("slot %q unexpectedly set", s.GetName())
		}
	}

	// Set creates a value...
	setResp, err := cli.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameAnthropicAPIKey, Value: "sk-test"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !setResp.GetCreated() {
		t.Errorf("created = false, want true on first Set")
	}

	// ...which List now reflects.
	resp, _ = cli.List(ctx, &secretsv1.ListRequest{})
	var anthropicSet bool
	for _, s := range resp.GetSecrets() {
		if s.GetName() == secrets.NameAnthropicAPIKey {
			anthropicSet = s.GetSet()
		}
	}
	if !anthropicSet {
		t.Errorf("anthropic slot not set after Set")
	}

	// Delete removes it.
	if _, err := cli.Delete(ctx, &secretsv1.DeleteRequest{Name: secrets.NameAnthropicAPIKey}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// A wrong token is rejected by the interceptor.
	if _, err := newClient("wrong").List(ctx, &secretsv1.ListRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("wrong token: code = %v, want Unauthenticated", status.Code(err))
	}
}

// TestTokenWhitespaceTolerance guards the common deployment footgun of a
// GRPC_AUTH_TOKEN / OGEN_GRPC_TOKEN env var pasted with surrounding whitespace.
// The server token is trimmed in New (so a trailing newline in Ogen's env is
// harmless), and the interceptor trims the incoming header (so a trailing space
// in Harbor's token — the only whitespace an HTTP/2 header can carry — still
// authenticates). A genuinely different token stays rejected.
func TestTokenWhitespaceTolerance(t *testing.T) {
	const clean = "shared-token"

	// Server configured as if GRPC_AUTH_TOKEN carried surrounding whitespace,
	// including a trailing newline — New trims it.
	srv, err := New("  "+clean+"\n", newFakeStore(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	// call dials a fresh client that sends "Bearer <tok>" and returns the List
	// status code.
	call := func(tok string) codes.Code {
		conn, err := grpc.NewClient(
			lis.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
				return invoker(ctx, method, req, reply, cc, opts...)
			}),
		)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		_, err = secretsv1.NewSecretsServiceClient(conn).List(t.Context(), &secretsv1.ListRequest{})
		return status.Code(err)
	}

	if code := call(clean); code != codes.OK {
		t.Errorf("clean client token: code = %v, want OK", code)
	}
	// A trailing space is the only whitespace an HTTP/2 header value can carry
	// (a newline is rejected by the client transport before send); the
	// interceptor's trim must let it through.
	if code := call(clean + " "); code != codes.OK {
		t.Errorf("space-padded client token: code = %v, want OK", code)
	}
	if code := call("nope"); code != codes.Unauthenticated {
		t.Errorf("wrong token: code = %v, want Unauthenticated", code)
	}
}
