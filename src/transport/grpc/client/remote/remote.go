// Package remote plans and decodes sidecar RPCs that river-service makes on
// ogen's behalf. It runs the ordinary typed sidecar clients over a connection
// that captures the request (Plan) or answers it with river-service's result
// (Decode), so request building, response mapping and error handling keep
// exactly one implementation, as zernio.Plan / zernio.Decode do for Zernio.
package remote

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Call is one planned sidecar RPC. For a unary method Request is the request
// message; for a client-streaming one it is the stream's first message, and
// the stored file follows it.
type Call struct {
	Method   string
	Request  []byte
	Streamed bool
}

// Result is river-service's answer to a Call: the gRPC status code and
// message, and on OK the response message.
type Result struct {
	Code    int
	Message string
	Body    []byte
}

// errCaptured stops a planned RPC before it is sent.
var errCaptured = errors.New("remote: rpc captured")

// Plan returns the RPC op would make, without making it. op runs one typed
// client method over conn; for a streaming upload it is given an empty file.
func Plan(op func(conn grpc.ClientConnInterface) error) (Call, error) {
	c := &captureConn{}
	err := op(c)
	switch {
	case c.call != nil:
		return *c.call, nil
	case err == nil:
		return Call{}, errors.New("remote: operation made no rpc")
	default:
		return Call{}, fmt.Errorf("remote: plan rpc: %w", err)
	}
}

// Decode runs op as if the sidecar had answered its RPC with res, so op's
// typed result and error (a gRPC status error for a non-OK code) are exactly
// what a direct call would have returned.
func Decode(res Result, op func(conn grpc.ClientConnInterface) error) error {
	return op(replayConn{res: res})
}

type captureConn struct{ call *Call }

func (c *captureConn) Invoke(_ context.Context, method string, args, _ any, _ ...grpc.CallOption) error {
	b, err := marshal(args)
	if err != nil {
		return err
	}
	c.call = &Call{Method: method, Request: b}
	return errCaptured
}

func (c *captureConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	if desc.ServerStreams {
		return nil, fmt.Errorf("remote: %s streams responses, which river-service does not relay", method)
	}
	return &captureStream{ctx: ctx, conn: c, method: method}, nil
}

// captureStream keeps the first message and refuses the rest, so a typed
// client stops before it reads the file it was handed.
type captureStream struct {
	ctx    context.Context
	conn   *captureConn
	method string
}

func (s *captureStream) SendMsg(m any) error {
	if s.conn.call != nil {
		return errCaptured
	}
	b, err := marshal(m)
	if err != nil {
		return err
	}
	s.conn.call = &Call{Method: s.method, Request: b, Streamed: true}
	return nil
}

func (s *captureStream) RecvMsg(any) error            { return errCaptured }
func (s *captureStream) CloseSend() error             { return nil }
func (s *captureStream) Header() (metadata.MD, error) { return nil, nil }
func (s *captureStream) Trailer() metadata.MD         { return nil }
func (s *captureStream) Context() context.Context     { return s.ctx }

type replayConn struct{ res Result }

func (c replayConn) Invoke(_ context.Context, _ string, _, reply any, _ ...grpc.CallOption) error {
	return c.answer(reply)
}

func (c replayConn) NewStream(ctx context.Context, _ *grpc.StreamDesc, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	return &replayStream{ctx: ctx, conn: c}, nil
}

func (c replayConn) answer(reply any) error {
	if c.res.Code != int(codes.OK) {
		return status.Error(codes.Code(c.res.Code), c.res.Message)
	}
	m, ok := reply.(proto.Message)
	if !ok {
		return fmt.Errorf("remote: reply %T is not a proto message", reply)
	}
	return proto.Unmarshal(c.res.Body, m)
}

// replayStream accepts what the typed client sends and answers with the
// result once it asks.
type replayStream struct {
	ctx  context.Context
	conn replayConn
}

func (s *replayStream) SendMsg(any) error            { return nil }
func (s *replayStream) RecvMsg(m any) error          { return s.conn.answer(m) }
func (s *replayStream) CloseSend() error             { return nil }
func (s *replayStream) Header() (metadata.MD, error) { return nil, nil }
func (s *replayStream) Trailer() metadata.MD         { return nil }
func (s *replayStream) Context() context.Context     { return s.ctx }

func marshal(m any) ([]byte, error) {
	pm, ok := m.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("remote: request %T is not a proto message", m)
	}
	return proto.Marshal(pm)
}
