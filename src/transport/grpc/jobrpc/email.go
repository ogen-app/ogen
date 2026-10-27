package jobrpc

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	jobrpcv1 "github.com/ogen-app/ogen/gen/jobrpc/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// EmailJobs is the part of the send_email worker the email RPCs drive.
// *queues.SendEmailProcessor implements it.
type EmailJobs interface {
	Prepare(ctx context.Context, t queues.SendEmailTask) (*queues.PreparedEmail, error)
	RecordOutcome(ctx context.Context, prep *queues.PreparedEmail, status models.EmailLogStatus, providerMsgID, errMsg string)
}

// emailJobs serves EmailJobService.
type emailJobs struct {
	jobrpcv1.UnimplementedEmailJobServiceServer
	jobs EmailJobs
}

func (e *emailJobs) PrepareEmail(ctx context.Context, req *jobrpcv1.PrepareEmailRequest) (*jobrpcv1.PrepareEmailResponse, error) {
	if e.jobs == nil {
		return nil, status.Error(codes.Unavailable, "email is not wired")
	}
	t, err := queues.SendEmailTaskFromArgs(req.GetArgs())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	prep, err := e.jobs.Prepare(ctx, t)
	if err != nil {
		slog.WarnContext(ctx, "prepare email failed; river-service will retry", logging.AttrComponent, "jobrpc", logging.AttrError, err)
		return nil, status.Error(codes.Unavailable, "prepare email failed")
	}
	if prep == nil {
		return &jobrpcv1.PrepareEmailResponse{Result: &jobrpcv1.PrepareEmailResponse_Settled{
			Settled: &jobrpcv1.EmailSettled{Reason: "settled in ogen; see its email log"},
		}}, nil
	}
	m := prep.Message
	return &jobrpcv1.PrepareEmailResponse{Result: &jobrpcv1.PrepareEmailResponse_Send{Send: &jobrpcv1.EmailMessage{
		From: m.From, ReplyTo: m.ReplyTo, To: m.To, Subject: m.Subject, Html: m.HTML, Text: m.Text,
		Headers: m.Headers, IdempotencyKey: m.IdempotencyKey,
	}}}, nil
}

var emailOutcomes = map[jobrpcv1.EmailOutcome]models.EmailLogStatus{
	jobrpcv1.EmailOutcome_EMAIL_OUTCOME_SENT:             models.EmailLogSent,
	jobrpcv1.EmailOutcome_EMAIL_OUTCOME_FAILED:           models.EmailLogFailed,
	jobrpcv1.EmailOutcome_EMAIL_OUTCOME_SKIPPED_DISABLED: models.EmailLogSkippedDisabled,
}

func (e *emailJobs) RecordEmailOutcome(ctx context.Context, req *jobrpcv1.RecordEmailOutcomeRequest) (*jobrpcv1.RecordEmailOutcomeResponse, error) {
	if e.jobs == nil {
		return nil, status.Error(codes.Unavailable, "email is not wired")
	}
	st, ok := emailOutcomes[req.GetOutcome()]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown email outcome %v", req.GetOutcome())
	}
	t, err := queues.SendEmailTaskFromArgs(req.GetArgs())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	m := req.GetMessage()
	msg := email.Message{
		From: m.GetFrom(), ReplyTo: m.GetReplyTo(), To: m.GetTo(), Subject: m.GetSubject(),
		HTML: m.GetHtml(), Text: m.GetText(), Headers: m.GetHeaders(), IdempotencyKey: t.IdempotencyKey,
	}
	if msg.To == "" {
		msg.To = t.ToEmail
	}
	e.jobs.RecordOutcome(ctx, queues.NewPreparedEmail(t, msg), st, req.GetProviderMessageId(), req.GetError())
	return &jobrpcv1.RecordEmailOutcomeResponse{}, nil
}
