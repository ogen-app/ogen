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
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// ZernioJobs answers the steps of the Zernio jobs river-service runs.
// *queues.ZernioSteps implements it.
type ZernioJobs interface {
	Step(ctx context.Context, in queues.ZernioStepInput) (queues.ZernioStepOutput, error)
}

// zernioJobs serves ZernioJobService.
type zernioJobs struct {
	jobrpcv1.UnimplementedZernioJobServiceServer
	steps ZernioJobs
}

func (z *zernioJobs) Step(ctx context.Context, req *jobrpcv1.StepRequest) (*jobrpcv1.StepResponse, error) {
	if z.steps == nil {
		return nil, status.Error(codes.Unavailable, "zernio jobs are not wired")
	}
	in := queues.ZernioStepInput{Kind: req.GetKind(), Args: req.GetArgs(), State: req.GetState()}
	if r := req.GetResult(); r != nil {
		in.Result = &zernio.Result{Status: int(r.GetStatus()), Body: r.GetBody(), Err: r.GetError()}
	}
	out, err := z.steps.Step(ctx, in)
	if errors.Is(err, queues.ErrUnknownZernioKind) {
		return nil, status.Errorf(codes.Unimplemented, "this ogen has no steps for job kind %q", req.GetKind())
	}
	if err != nil {
		slog.WarnContext(ctx, "zernio step failed; river-service will retry", logging.AttrComponent, "jobrpc", "job_kind", req.GetKind(), logging.AttrError, err)
		return nil, status.Error(codes.Unavailable, "zernio step failed")
	}
	return stepResponse(out), nil
}

func stepResponse(out queues.ZernioStepOutput) *jobrpcv1.StepResponse {
	resp := &jobrpcv1.StepResponse{State: out.State}
	switch {
	case out.Call != nil:
		resp.Next = &jobrpcv1.StepResponse_Call{Call: zernioCall(*out.Call)}
	case out.Retry != nil:
		r := &jobrpcv1.ZernioStepRetry{Reason: out.Retry.Reason}
		if out.Retry.After > 0 {
			r.After = durationpb.New(out.Retry.After)
		}
		resp.Next = &jobrpcv1.StepResponse_Retry{Retry: r}
	default:
		d := &jobrpcv1.ZernioStepDone{}
		if out.Done != nil {
			d.Outcome = out.Done.Outcome
			for _, c := range out.Done.Enqueue {
				d.Enqueue = append(d.Enqueue, &jobrpcv1.ChildJob{Kind: c.Kind, Args: c.Args})
			}
		}
		resp.Next = &jobrpcv1.StepResponse_Done{Done: d}
	}
	return resp
}

func zernioCall(c zernio.Call) *jobrpcv1.ZernioCall {
	return &jobrpcv1.ZernioCall{Method: c.Method, Path: c.Path, Query: queryParams(c.Query), Headers: c.Headers, Body: c.Body}
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
