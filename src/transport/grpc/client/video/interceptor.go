package video

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Correlation metadata keys. They MUST match video-service's request/tenant id
// header names byte-for-byte (gRPC lowercases metadata keys) so one request
// traces end-to-end across the gRPC boundary: API request -> video-service.
// Transport-level only — no proto/message change.
const (
	requestIDHeader = "x-request-id"
	tenantIDHeader  = "x-tenant-id"
)

// withCorrelation copies the request id and tenant id carried by ctx into the
// outgoing gRPC metadata so video-service enriches its logs with the same ids
// the API already logs. Absent ids are skipped: video-service then generates
// its own request id and logs no tenant, so the call still succeeds — this
// only closes the log-correlation loop.
func withCorrelation(ctx context.Context) context.Context {
	var kv []string
	if id, ok := logging.RequestIDFrom(ctx); ok {
		kv = append(kv, requestIDHeader, id)
	}
	if id, ok := tenantctx.From(ctx); ok {
		kv = append(kv, tenantIDHeader, id)
	}
	if len(kv) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}

// correlationUnaryInterceptor injects the correlation metadata on every unary
// call (Probe is unary).
func correlationUnaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return invoker(withCorrelation(ctx), method, req, reply, cc, opts...)
}

// correlationStreamInterceptor is the client-streaming counterpart, so any
// future streaming RPC on this connection carries the same ids.
func correlationStreamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return streamer(withCorrelation(ctx), desc, cc, method, opts...)
}
