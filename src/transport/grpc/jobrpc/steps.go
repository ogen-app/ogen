package jobrpc

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	jobrpcv1 "github.com/ogen-app/ogen/gen/jobrpc/v1"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// JobSteps answers the steps of the jobs river-service runs step by step.
// *queues.JobSteps implements it.
type JobSteps interface {
	Step(ctx context.Context, in queues.StepInput) (queues.StepOutput, error)
}

// jobSteps serves JobStepService.
type jobSteps struct {
	jobrpcv1.UnimplementedJobStepServiceServer
	steps JobSteps
}

func (j *jobSteps) Step(ctx context.Context, req *jobrpcv1.StepRequest) (*jobrpcv1.StepResponse, error) {
	if j.steps == nil {
		return nil, status.Error(codes.Unavailable, "job steps are not wired")
	}
	in := queues.StepInput{
		Kind: req.GetKind(), Args: req.GetArgs(), State: req.GetState(),
		Attempt: int(req.GetAttempt()), MaxAttempts: int(req.GetMaxAttempts()),
	}
	if r := req.GetResult(); r != nil {
		in.Result = &queues.StepResult{
			Status: int(r.GetStatus()), Body: r.GetBody(), Err: r.GetError(),
			CredentialMissing: r.GetCredentialMissing(), ContentType: r.GetContentType(),
		}
	}
	out, err := j.steps.Step(ctx, in)
	if errors.Is(err, queues.ErrUnknownStepKind) {
		return nil, status.Errorf(codes.Unimplemented, "this ogen has no steps for job kind %q", req.GetKind())
	}
	if err != nil {
		slog.WarnContext(ctx, "job step failed; river-service will retry", logging.AttrComponent, "jobrpc", "job_kind", req.GetKind(), logging.AttrError, err)
		return nil, status.Error(codes.Unavailable, "job step failed")
	}
	return stepResponse(out), nil
}

func stepResponse(out queues.StepOutput) *jobrpcv1.StepResponse {
	resp := &jobrpcv1.StepResponse{State: out.State}
	switch {
	case out.HTTP != nil:
		c := out.HTTP
		resp.Next = &jobrpcv1.StepResponse_Http{Http: &jobrpcv1.HttpCall{
			Upstream: c.Upstream, Method: c.Method, Path: c.Path, Query: queryParams(c.Query),
			Headers: c.Headers, Body: c.Body, Url: c.URL,
		}}
	case out.Sidecar != nil:
		c := out.Sidecar
		sc := &jobrpcv1.SidecarCall{Service: c.Service, Method: c.Method, Request: c.Request}
		if c.Stream != nil {
			sc.Stream = &jobrpcv1.StreamSource{SourceUrl: c.Stream.SourceURL, Header: c.Stream.Header, ChunkField: c.Stream.ChunkField}
		}
		if c.Timeout > 0 {
			sc.Timeout = durationpb.New(c.Timeout)
		}
		resp.Next = &jobrpcv1.StepResponse_Sidecar{Sidecar: sc}
	case out.Transfer != nil:
		t := out.Transfer
		resp.Next = &jobrpcv1.StepResponse_Transfer{Transfer: &jobrpcv1.MediaTransfer{
			SourceUrl: t.SourceURL, UploadUrl: t.UploadURL, ContentType: t.ContentType, Size: t.Size,
		}}
	case out.Wait > 0:
		resp.Next = &jobrpcv1.StepResponse_Wait{Wait: durationpb.New(out.Wait)}
	case out.Retry != nil:
		r := &jobrpcv1.StepRetry{Reason: out.Retry.Reason}
		if out.Retry.After > 0 {
			r.After = durationpb.New(out.Retry.After)
		}
		resp.Next = &jobrpcv1.StepResponse_Retry{Retry: r}
	default:
		d := &jobrpcv1.StepDone{}
		if out.Done != nil {
			d.Outcome = out.Done.Outcome
			for _, c := range out.Done.Enqueue {
				d.Enqueue = append(d.Enqueue, &jobrpcv1.ChildJob{Kind: c.Kind, Args: c.Args})
			}
			for _, m := range out.Done.Cancel {
				jm := &jobrpcv1.JobMatch{Kind: m.Kind}
				for _, a := range m.Args {
					jm.Args = append(jm.Args, &jobrpcv1.ArgMatch{Key: a.Key, Values: a.Values})
				}
				d.Cancel = append(d.Cancel, jm)
			}
		}
		resp.Next = &jobrpcv1.StepResponse_Done{Done: d}
	}
	return resp
}

// queryParams flattens the query in key order, so a call's wire form is
// stable.
func queryParams(q url.Values) []*jobrpcv1.QueryParam {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []*jobrpcv1.QueryParam
	for _, k := range keys {
		for _, v := range q[k] {
			out = append(out, &jobrpcv1.QueryParam{Key: k, Value: v})
		}
	}
	return out
}
