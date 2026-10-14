package jobrpc

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	jobrpcv1 "github.com/ogen-app/ogen/gen/jobrpc/v1"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

type fakeSteps struct {
	out queues.ZernioStepOutput
	err error
	got queues.ZernioStepInput
}

func (f *fakeSteps) Step(_ context.Context, in queues.ZernioStepInput) (queues.ZernioStepOutput, error) {
	f.got = in
	return f.out, f.err
}

func TestZernioStep(t *testing.T) {
	steps := &fakeSteps{out: queues.ZernioStepOutput{State: []byte(`{"phase":"list"}`), Call: &zernio.Call{
		Method: "GET", Path: "/accounts/health", Query: url.Values{"profileId": {"p1"}, "a": {"1"}},
	}}}
	c := jobrpcv1.NewZernioJobServiceClient(dial(t, Deps{Zernio: steps}))
	ctx := authed(t.Context(), token)

	res, err := c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k", Args: []byte(`{}`), Result: &jobrpcv1.ZernioResult{Status: 404, Body: []byte(`x`)}})
	if err != nil {
		t.Fatal(err)
	}
	call := res.GetCall()
	if call.GetPath() != "/accounts/health" || len(call.GetQuery()) != 2 || call.GetQuery()[0].GetKey() != "a" || string(res.GetState()) != `{"phase":"list"}` {
		t.Errorf("response = %v", res)
	}
	if steps.got.Result == nil || steps.got.Result.Status != 404 {
		t.Errorf("input result = %+v", steps.got.Result)
	}

	steps.out = queues.ZernioStepOutput{Done: &queues.ZernioStepDone{Outcome: "fanned_out", Enqueue: []queues.ChildJob{{Kind: "c", Args: []byte(`{"tenant_id":"t1"}`)}}}}
	res, _ = c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k"})
	if d := res.GetDone(); d.GetOutcome() != "fanned_out" || len(d.GetEnqueue()) != 1 {
		t.Errorf("done = %v", d)
	}

	steps.out = queues.ZernioStepOutput{Retry: &queues.ZernioStepRetry{Reason: "down", After: time.Minute}}
	res, _ = c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k"})
	if r := res.GetRetry(); r.GetReason() != "down" || r.GetAfter().AsDuration() != time.Minute {
		t.Errorf("retry = %v", r)
	}

	steps.err = queues.ErrUnknownZernioKind
	if _, err := c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("unknown kind = %v, want Unimplemented", err)
	}
	steps.err = errors.New("db down")
	if _, err := c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k"}); status.Code(err) != codes.Unavailable {
		t.Errorf("db failure = %v, want Unavailable", err)
	}
}
