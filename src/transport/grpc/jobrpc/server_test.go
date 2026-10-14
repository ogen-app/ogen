package jobrpc

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	jobrpcv1 "github.com/ogen-app/ogen/gen/jobrpc/v1"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

type fakeSecrets struct {
	secrets.Store
	values map[string]string
}

func (f fakeSecrets) Get(_ context.Context, name string) (string, error) { return f.values[name], nil }

const token = "river-token"

func dial(t *testing.T, d Deps) *grpc.ClientConn {
	t.Helper()
	d.Token = token
	srv, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func authed(ctx context.Context, tok string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

func TestNewRequiresAToken(t *testing.T) {
	if _, err := New(Deps{Token: "  "}); err == nil {
		t.Fatal("New with an empty token succeeded")
	}
}

func TestAuth(t *testing.T) {
	conn := dial(t, Deps{Maintenance: map[string]func(context.Context) error{"cleanup_post_logs": func(context.Context) error { return nil }}})
	m := jobrpcv1.NewMaintenanceServiceClient(conn)
	req := &jobrpcv1.RunRequest{Task: "cleanup_post_logs"}
	tests := []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{name: "no token", ctx: t.Context(), want: codes.Unauthenticated},
		{name: "harbor's token", ctx: authed(t.Context(), "harbor-token"), want: codes.Unauthenticated},
		{name: "river-service's token", ctx: authed(t.Context(), token), want: codes.OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := m.Run(tt.ctx, req); status.Code(err) != tt.want {
				t.Errorf("Run() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestStreamGateAuthenticates(t *testing.T) {
	sem := make(chan struct{}, 1)
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer wrong"))
	err := streamGate(token, sem)(nil, ctxStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: "/jobrpc.v1.X/Stream"},
		func(any, grpc.ServerStream) error { return nil })
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("stream with a wrong token = %v, want Unauthenticated", err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	conn := dial(t, Deps{MaxConcurrency: 1, Maintenance: map[string]func(context.Context) error{
		"cleanup_post_logs": func(context.Context) error {
			close(entered)
			<-release
			return nil
		},
	}})
	m := jobrpcv1.NewMaintenanceServiceClient(conn)
	done := make(chan error, 1)
	go func() {
		_, err := m.Run(authed(t.Context(), token), &jobrpcv1.RunRequest{Task: "cleanup_post_logs"})
		done <- err
	}()
	<-entered
	if _, err := m.Run(authed(t.Context(), token), &jobrpcv1.RunRequest{Task: "cleanup_post_logs"}); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("call over the limit = %v, want ResourceExhausted", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first call: %v", err)
	}
}

func TestLease(t *testing.T) {
	conn := dial(t, Deps{LeaseTTL: time.Minute, Secrets: fakeSecrets{values: map[string]string{
		secrets.NameResendAPIKey:    "re_123",
		secrets.NameAnthropicAPIKey: "sk-ant",
	}}})
	c := jobrpcv1.NewCredentialServiceClient(conn)
	ctx := authed(t.Context(), token)

	res, err := c.Lease(ctx, &jobrpcv1.LeaseRequest{Name: secrets.NameResendAPIKey})
	if err != nil || res.GetValue() != "re_123" {
		t.Fatalf("Lease(resend) = %v, %v", res, err)
	}
	if left := time.Until(res.GetExpiresAt().AsTime()); left <= 0 || left > time.Minute {
		t.Errorf("lease expires in %v, want within a minute", left)
	}
	if _, err := c.Lease(ctx, &jobrpcv1.LeaseRequest{Name: secrets.NameAnthropicAPIKey}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Lease(anthropic) = %v, want PermissionDenied", err)
	}
	if _, err := c.Lease(ctx, &jobrpcv1.LeaseRequest{Name: secrets.NameZernioAPIKey}); status.Code(err) != codes.NotFound {
		t.Errorf("Lease(unset zernio) = %v, want NotFound", err)
	}
}

func TestMaintenanceRun(t *testing.T) {
	var gotRequestID string
	conn := dial(t, Deps{Maintenance: map[string]func(context.Context) error{
		"cleanup_post_logs": func(ctx context.Context) error {
			gotRequestID, _ = logging.RequestIDFrom(ctx)
			return nil
		},
		"cleanup_notifications": func(context.Context) error { return context.DeadlineExceeded },
	}})
	m := jobrpcv1.NewMaintenanceServiceClient(conn)
	ctx := metadata.AppendToOutgoingContext(authed(t.Context(), token), requestIDHeader, "req-7")

	if _, err := m.Run(ctx, &jobrpcv1.RunRequest{Task: "cleanup_post_logs"}); err != nil {
		t.Fatal(err)
	}
	if gotRequestID != "req-7" {
		t.Errorf("request id in the sweep = %q, want req-7", gotRequestID)
	}
	if _, err := m.Run(ctx, &jobrpcv1.RunRequest{Task: "cleanup_notifications"}); status.Code(err) != codes.Internal {
		t.Errorf("failing sweep = %v, want Internal", err)
	}
	if _, err := m.Run(ctx, &jobrpcv1.RunRequest{Task: "drop_everything"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown task = %v, want InvalidArgument", err)
	}
}
