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
	"github.com/ogen-app/ogen/src/jobs/queues"
)

type fakeSteps struct {
	out queues.StepOutput
	err error
	got queues.StepInput
}

func (f *fakeSteps) Step(_ context.Context, in queues.StepInput) (queues.StepOutput, error) {
	f.got = in
	return f.out, f.err
}

func TestJobStep(t *testing.T) {
	steps := &fakeSteps{out: queues.StepOutput{State: []byte(`{"phase":"list"}`), HTTP: &queues.HTTPCall{
		Upstream: queues.UpstreamZernio, Method: "GET", Path: "/accounts/health", Query: url.Values{"profileId": {"p1"}, "a": {"1"}},
	}}}
	c := jobrpcv1.NewJobStepServiceClient(dial(t, Deps{Steps: steps}))
	ctx := authed(t.Context(), token)

	res, err := c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k", Args: []byte(`{}`), Result: &jobrpcv1.StepResult{Status: 404, Body: []byte(`x`), CredentialMissing: true}})
	if err != nil {
		t.Fatal(err)
	}
	call := res.GetHttp()
	if call.GetUpstream() != "zernio" || call.GetPath() != "/accounts/health" || len(call.GetQuery()) != 2 || call.GetQuery()[0].GetKey() != "a" || string(res.GetState()) != `{"phase":"list"}` {
		t.Errorf("response = %v", res)
	}
	if steps.got.Result == nil || steps.got.Result.Status != 404 || !steps.got.Result.CredentialMissing {
		t.Errorf("input result = %+v", steps.got.Result)
	}

	steps.out = queues.StepOutput{Sidecar: &queues.SidecarCall{Service: "pdf", Method: "/pdf.v1.PdfService/Parse", Stream: &queues.StreamSource{SourceURL: "s", Header: []byte{1}, ChunkField: 2}, Timeout: time.Minute}}
	res, _ = c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k"})
	if sc := res.GetSidecar(); sc.GetService() != "pdf" || sc.GetStream().GetChunkField() != 2 || sc.GetTimeout().AsDuration() != time.Minute {
		t.Errorf("sidecar = %v", sc)
	}

	steps.out = queues.StepOutput{Done: &queues.StepDone{
		Outcome: "fanned_out", Enqueue: []queues.ChildJob{{Kind: "c", Args: []byte(`{"tenant_id":"t1"}`)}},
		Cancel: []queues.JobMatch{{Kind: "poll", Args: []queues.ArgMatch{{Key: "post_id", Values: []string{"p1"}}}}},
	}}
	res, _ = c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k", Attempt: 3, MaxAttempts: 5})
	if d := res.GetDone(); d.GetOutcome() != "fanned_out" || len(d.GetEnqueue()) != 1 ||
		len(d.GetCancel()) != 1 || d.GetCancel()[0].GetArgs()[0].GetValues()[0] != "p1" {
		t.Errorf("done = %v", d)
	}
	if steps.got.Attempt != 3 || steps.got.MaxAttempts != 5 {
		t.Errorf("attempt = %d of %d, want 3 of 5", steps.got.Attempt, steps.got.MaxAttempts)
	}

	steps.out = queues.StepOutput{Retry: &queues.StepRetry{Reason: "down", After: time.Minute}}
	res, _ = c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k"})
	if r := res.GetRetry(); r.GetReason() != "down" || r.GetAfter().AsDuration() != time.Minute {
		t.Errorf("retry = %v", r)
	}

	steps.err = queues.ErrUnknownStepKind
	if _, err := c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("unknown kind = %v, want Unimplemented", err)
	}
	steps.err = errors.New("db down")
	if _, err := c.Step(ctx, &jobrpcv1.StepRequest{Kind: "k"}); status.Code(err) != codes.Unavailable {
		t.Errorf("db failure = %v, want Unavailable", err)
	}
}
