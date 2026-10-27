package jobrpc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	jobrpcv1 "github.com/ogen-app/ogen/gen/jobrpc/v1"
	jobsv1 "github.com/ogen-app/ogen/gen/jobs/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

type fakeEmailJobs struct {
	prep     *queues.PreparedEmail
	err      error
	recorded []*queues.PreparedEmail
	statuses []models.EmailLogStatus
	gotTask  queues.SendEmailTask
}

func (f *fakeEmailJobs) Prepare(_ context.Context, t queues.SendEmailTask) (*queues.PreparedEmail, error) {
	f.gotTask = t
	return f.prep, f.err
}

func (f *fakeEmailJobs) RecordOutcome(_ context.Context, prep *queues.PreparedEmail, st models.EmailLogStatus, _, _ string) {
	f.recorded = append(f.recorded, prep)
	f.statuses = append(f.statuses, st)
}

func args() *jobsv1.SendEmailArgs {
	return &jobsv1.SendEmailArgs{UserId: "u1", TenantId: "t1", TemplateKey: "welcome", EmailKind: "transactional", IdempotencyKey: "welcome:u1"}
}

func TestPrepareEmail(t *testing.T) {
	jobs := &fakeEmailJobs{prep: &queues.PreparedEmail{Message: email.Message{To: "a@x.test", Subject: "Hi", Headers: map[string]string{"X": "1"}, IdempotencyKey: "welcome:u1"}}}
	conn := dial(t, Deps{Email: jobs})
	c := jobrpcv1.NewEmailJobServiceClient(conn)
	ctx := authed(t.Context(), token)

	res, err := c.PrepareEmail(ctx, &jobrpcv1.PrepareEmailRequest{Args: args()})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.GetSend(); m.GetTo() != "a@x.test" || m.GetHeaders()["X"] != "1" || m.GetIdempotencyKey() != "welcome:u1" {
		t.Errorf("send = %v", m)
	}
	if jobs.gotTask.EmailKind != models.EmailKindTransactional || jobs.gotTask.TemplateKey != "welcome" {
		t.Errorf("task = %+v", jobs.gotTask)
	}

	jobs.prep = nil
	if res, err := c.PrepareEmail(ctx, &jobrpcv1.PrepareEmailRequest{Args: args()}); err != nil || res.GetSettled() == nil {
		t.Errorf("settled job = %v, %v", res, err)
	}

	jobs.err = errors.New("db down")
	if _, err := c.PrepareEmail(ctx, &jobrpcv1.PrepareEmailRequest{Args: args()}); status.Code(err) != codes.Unavailable {
		t.Errorf("transient failure = %v, want Unavailable", err)
	}
}

func TestRecordEmailOutcome(t *testing.T) {
	jobs := &fakeEmailJobs{}
	conn := dial(t, Deps{Email: jobs})
	c := jobrpcv1.NewEmailJobServiceClient(conn)
	ctx := authed(t.Context(), token)

	if _, err := c.RecordEmailOutcome(ctx, &jobrpcv1.RecordEmailOutcomeRequest{
		Args: args(), Outcome: jobrpcv1.EmailOutcome_EMAIL_OUTCOME_SENT, ProviderMessageId: "msg_1",
		Message: &jobrpcv1.EmailMessage{To: "a@x.test", Subject: "Hi", Html: "<p>Hi</p>"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(jobs.recorded) != 1 || jobs.statuses[0] != models.EmailLogSent {
		t.Fatalf("recorded %v %v", jobs.recorded, jobs.statuses)
	}
	if got := jobs.recorded[0]; got.Log.ToEmail != "a@x.test" || got.Log.IdempotencyKey != "welcome:u1" || got.Rendered.HTML != "<p>Hi</p>" {
		t.Errorf("prepared = %+v", got)
	}
	if _, err := c.RecordEmailOutcome(ctx, &jobrpcv1.RecordEmailOutcomeRequest{Args: args()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no outcome = %v, want InvalidArgument", err)
	}
	// A user's address is only known from the prepared message; without it
	// the outcome would be logged against nobody.
	if _, err := c.RecordEmailOutcome(ctx, &jobrpcv1.RecordEmailOutcomeRequest{
		Args: args(), Outcome: jobrpcv1.EmailOutcome_EMAIL_OUTCOME_SKIPPED_DISABLED,
	}); status.Code(err) != codes.InvalidArgument || len(jobs.recorded) != 1 {
		t.Errorf("no recipient = %v (recorded %d), want InvalidArgument and nothing recorded", err, len(jobs.recorded))
	}
}
