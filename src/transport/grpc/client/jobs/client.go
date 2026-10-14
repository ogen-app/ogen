// Package jobs is the client for river-service's jobs.v1 JobService, which the
// outbox relay delivers jobs to.
package jobs

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	jobsv1 "github.com/ogen-app/ogen/gen/jobs/v1"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// requestIDHeader matches river-service's request-id metadata key.
const requestIDHeader = "x-request-id"

// roundRobin spreads calls across every river-service replica the DNS name
// resolves to, instead of pinning one connection to one replica.
const roundRobin = `{"loadBalancingConfig":[{"round_robin":{}}]}`

// ErrDisabled is returned by a nil client (no RIVER_SERVICE_ADDR).
var ErrDisabled = errors.New("jobs: river-service is not configured")

// Config wires a Client.
type Config struct {
	// Addr is the gRPC target, e.g. dns:///river-service.railway.internal:9092.
	// Empty disables the client.
	Addr string
	// Token is the bearer token river-service expects.
	Token string
}

// Client is a thin JobService client, safe for concurrent use. A nil *Client
// is the disabled sentinel and every method returns ErrDisabled.
type Client struct {
	conn  *grpc.ClientConn
	rpc   jobsv1.JobServiceClient
	token string
}

// New dials river-service. It connects lazily, so it never blocks on the
// service being up. An empty Addr returns a nil client.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, nil
	}
	conn, err := grpc.NewClient(cfg.Addr,
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(roundRobin),
	)
	if err != nil {
		return nil, fmt.Errorf("jobs: dial %q: %w", cfg.Addr, err)
	}
	return &Client{conn: conn, rpc: jobsv1.NewJobServiceClient(conn), token: cfg.Token}, nil
}

// Close releases the connection. Safe on a nil client.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Enqueue hands one job to river-service.
func (c *Client) Enqueue(ctx context.Context, req *jobsv1.EnqueueRequest) (*jobsv1.EnqueueResponse, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	return c.rpc.Enqueue(c.outgoing(ctx), req)
}

// outgoing adds the bearer token and the request id to the call's metadata.
func (c *Client) outgoing(ctx context.Context) context.Context {
	kv := []string{"authorization", "Bearer " + c.token}
	if id, ok := logging.RequestIDFrom(ctx); ok {
		kv = append(kv, requestIDHeader, id)
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}
