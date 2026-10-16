// Package jobrpc serves jobrpc.v1, the RPCs river-service's jobs call on
// ogen: credential leases, housekeeping sweeps, email sends and the steps of
// the jobs that need outside calls. It runs on its own
// listener with its own bearer token, so neither Harbor's token nor
// river-service's opens the other's services, and with a concurrency limit, so
// a backlog of jobs can't starve request handling of database connections.
package jobrpc

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	jobrpcv1 "github.com/ogen-app/ogen/gen/jobrpc/v1"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const (
	healthPrefix = "/grpc.health.v1.Health/"
	// requestIDHeader matches river-service's outgoing request-id key.
	requestIDHeader = "x-request-id"
	// maxConnectionAge makes river-service reconnect periodically, so its
	// calls spread across ogen replicas after a scale-up.
	maxConnectionAge      = 5 * time.Minute
	maxConnectionAgeGrace = 30 * time.Second
)

// leasable are the secret-store credentials river-service may lease: the
// vendor keys its jobs call vendors with. Everything else stays in ogen.
var leasable = map[string]bool{
	secrets.NameZernioAPIKey:    true,
	secrets.NameGeminiAPIKey:    true,
	secrets.NameResendAPIKey:    true,
	secrets.NameFirecrawlAPIKey: true,
}

// Deps wires the server.
type Deps struct {
	// Token is the bearer token river-service sends (RIVER_RPC_AUTH_TOKEN).
	Token string
	// MaxConcurrency caps job RPCs in flight; past it calls get
	// RESOURCE_EXHAUSTED. Zero or less means 8.
	MaxConcurrency int
	Secrets        secrets.Store
	// LeaseTTL is how long a leased credential is good for. Zero means 5m.
	LeaseTTL time.Duration
	// ConfigCredentials are leasable keys ogen holds in its own config rather
	// than the secret store, by lease name (the Harbor webhook signing key).
	ConfigCredentials map[string]string
	// Maintenance runs one pass of each housekeeping sweep, by kind.
	Maintenance map[string]func(context.Context) error
	// Email prepares and records the sends river-service makes.
	Email EmailJobs
	// Steps answers the steps of the jobs river-service runs step by step.
	Steps JobSteps
}

// New builds the job-RPC server. An empty token is rejected: the caller must
// not start the listener rather than run it open. So is a server missing one
// of the services river-service's jobs need: every job of that kind would
// fail on it, so it fails the boot instead.
func New(d Deps) (*grpc.Server, error) {
	token := strings.TrimSpace(d.Token)
	if token == "" {
		return nil, errors.New("jobrpc: RIVER_RPC_AUTH_TOKEN is required")
	}
	switch {
	case d.Steps == nil:
		return nil, errors.New("jobrpc: job steps are not wired")
	case d.Maintenance == nil:
		return nil, errors.New("jobrpc: maintenance tasks are not wired")
	case d.Email == nil:
		return nil, errors.New("jobrpc: email jobs are not wired")
	}
	limit := d.MaxConcurrency
	if limit <= 0 {
		limit = 8
	}
	sem := make(chan struct{}, limit)
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(unaryGate(token, sem)),
		grpc.ChainStreamInterceptor(streamGate(token, sem)),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: maxConnectionAge, MaxConnectionAgeGrace: maxConnectionAgeGrace}),
	)
	ttl := d.LeaseTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	jobrpcv1.RegisterCredentialServiceServer(srv, &credentials{store: d.Secrets, config: d.ConfigCredentials, ttl: ttl})
	jobrpcv1.RegisterMaintenanceServiceServer(srv, &maintenance{tasks: d.Maintenance})
	jobrpcv1.RegisterEmailJobServiceServer(srv, &emailJobs{jobs: d.Email})
	jobrpcv1.RegisterJobStepServiceServer(srv, &jobSteps{steps: d.Steps})
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return srv, nil
}

// gate authenticates a call, attaches the caller's request id, and takes a
// concurrency slot, or refuses. Health checks pass untouched.
func gate(ctx context.Context, method, token string, sem chan struct{}) (context.Context, func(), error) {
	if strings.HasPrefix(method, healthPrefix) {
		return ctx, func() {}, nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	if !validToken(md.Get("authorization"), token) {
		return nil, nil, status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	if ids := md.Get(requestIDHeader); len(ids) > 0 && ids[0] != "" {
		ctx = logging.WithRequestID(ctx, ids[0])
	}
	select {
	case sem <- struct{}{}:
		return ctx, func() { <-sem }, nil
	default:
		return nil, nil, status.Error(codes.ResourceExhausted, "job RPCs are at their concurrency limit; retry later")
	}
}

func validToken(vals []string, token string) bool {
	want := []byte("Bearer " + token)
	for _, v := range vals {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(v)), want) == 1 {
			return true
		}
	}
	return false
}

func unaryGate(token string, sem chan struct{}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, release, err := gate(ctx, info.FullMethod, token, sem)
		if err != nil {
			return nil, err
		}
		defer release()
		return handler(ctx, req)
	}
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context { return s.ctx }

func streamGate(token string, sem chan struct{}) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, release, err := gate(ss.Context(), info.FullMethod, token, sem)
		if err != nil {
			return err
		}
		defer release()
		return handler(srv, ctxStream{ServerStream: ss, ctx: ctx})
	}
}

// credentials serves CredentialService.
type credentials struct {
	jobrpcv1.UnimplementedCredentialServiceServer
	store  secrets.Store
	config map[string]string
	ttl    time.Duration
}

func (c *credentials) Lease(ctx context.Context, req *jobrpcv1.LeaseRequest) (*jobrpcv1.LeaseResponse, error) {
	name := req.GetName()
	if value, ok := c.config[name]; ok {
		if value == "" {
			return nil, status.Errorf(codes.NotFound, "credential %q is not set", name)
		}
		return &jobrpcv1.LeaseResponse{Value: value, ExpiresAt: timestamppb.New(time.Now().Add(c.ttl))}, nil
	}
	if !leasable[name] {
		return nil, status.Errorf(codes.PermissionDenied, "credential %q is not leasable", name)
	}
	if c.store == nil {
		return nil, status.Error(codes.Unavailable, "secret store unavailable")
	}
	value, err := c.store.Get(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "credential lease failed", logging.AttrComponent, "jobrpc", "credential", name, logging.AttrError, err)
		return nil, status.Error(codes.Unavailable, "credential lookup failed")
	}
	if value == "" {
		return nil, status.Errorf(codes.NotFound, "credential %q is not set", name)
	}
	return &jobrpcv1.LeaseResponse{Value: value, ExpiresAt: timestamppb.New(time.Now().Add(c.ttl))}, nil
}

// maintenance serves MaintenanceService.
type maintenance struct {
	jobrpcv1.UnimplementedMaintenanceServiceServer
	tasks map[string]func(context.Context) error
}

func (m *maintenance) Run(ctx context.Context, req *jobrpcv1.RunRequest) (*jobrpcv1.RunResponse, error) {
	run, ok := m.tasks[req.GetTask()]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown maintenance task %q", req.GetTask())
	}
	if err := run(ctx); err != nil {
		slog.ErrorContext(ctx, "maintenance task failed", logging.AttrComponent, "jobrpc", "task", req.GetTask(), logging.AttrError, err)
		return nil, status.Errorf(codes.Internal, "maintenance task %q failed", req.GetTask())
	}
	return &jobrpcv1.RunResponse{}, nil
}
