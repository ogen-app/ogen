package queues

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	jobsv1 "github.com/ogen-app/ogen/gen/jobs/v1"
	"github.com/ogen-app/ogen/src/domain/models"
)

// enqueueRequest converts a job's args to the jobs.v1 request river-service
// receives: the matching args message, field for field, and the schedule from
// opts. Queue, attempts and uniqueness are river-service's own per kind.
func enqueueRequest(args JobArgs, opts *EnqueueOpts) (*jobsv1.EnqueueRequest, error) {
	req := &jobsv1.EnqueueRequest{}
	if opts != nil && !opts.ScheduledAt.IsZero() {
		req.ScheduledAt = timestamppb.New(opts.ScheduledAt)
	}
	ok, err := setArgs(req, args)
	if err != nil {
		return nil, fmt.Errorf("jobs: %s args: %w", args.Kind(), err)
	}
	if !ok {
		return nil, fmt.Errorf("jobs: %s has no jobs.v1 args message", args.Kind())
	}
	return req, nil
}

// setArgs fills req's args oneof from args and reports whether the kind has a
// jobs.v1 message.
func setArgs(req *jobsv1.EnqueueRequest, args JobArgs) (bool, error) {
	switch a := args.(type) {
	case SubmitPostTask:
		req.Args = &jobsv1.EnqueueRequest_SubmitPostToZernio{SubmitPostToZernio: &jobsv1.SubmitPostToZernioArgs{PostId: a.PostID}}
	case PollZernioStatusTask:
		req.Args = &jobsv1.EnqueueRequest_PollZernioStatus{PollZernioStatus: &jobsv1.PollZernioStatusArgs{PostId: a.PostID, PublisherPostId: a.PublisherPostID}}
	case CancelZernioJobTask:
		req.Args = &jobsv1.EnqueueRequest_CancelZernioJob{CancelZernioJob: &jobsv1.CancelZernioJobArgs{PostId: a.PostID, Target: string(a.Target), Actor: a.Actor}}
	case WithdrawZernioPostTask:
		req.Args = &jobsv1.EnqueueRequest_WithdrawZernioPost{WithdrawZernioPost: &jobsv1.WithdrawZernioPostArgs{
			PublisherPostId: a.PublisherPostID, PostId: a.PostID, TenantId: a.TenantID, Reason: a.Reason, Actor: a.Actor,
		}}
	case PostFirstCommentTask:
		req.Args = &jobsv1.EnqueueRequest_PostFirstComment{PostFirstComment: &jobsv1.PostFirstCommentArgs{PostId: a.PostID, PublisherPostId: a.PublisherPostID}}
	case BootstrapZernioProfileTask:
		req.Args = &jobsv1.EnqueueRequest_BootstrapZernioProfile{BootstrapZernioProfile: &jobsv1.BootstrapZernioProfileArgs{TenantId: a.TenantID}}
	case TeardownZernioProfileTask:
		req.Args = &jobsv1.EnqueueRequest_TeardownZernioProfile{TeardownZernioProfile: &jobsv1.TeardownZernioProfileArgs{TenantId: a.TenantID, Reason: a.Reason}}
	case SendEmailTask:
		m, err := SendEmailArgs(a)
		if err != nil {
			return false, err
		}
		req.Args = &jobsv1.EnqueueRequest_SendEmail{SendEmail: m}
	case NotifyHarborTenantRegisteredTask:
		req.Args = &jobsv1.EnqueueRequest_NotifyHarborTenantRegistered{NotifyHarborTenantRegistered: &jobsv1.NotifyHarborTenantRegisteredArgs{TenantId: a.TenantID}}
	case BuildWeeklyReportTask:
		req.Args = &jobsv1.EnqueueRequest_BuildWeeklyReport{BuildWeeklyReport: &jobsv1.BuildWeeklyReportArgs{TenantId: a.TenantID, WeekStart: a.WeekStart}}
	case BillingScheduledChangeTask:
		req.Args = &jobsv1.EnqueueRequest_BillingApplyScheduledChange{BillingApplyScheduledChange: &jobsv1.BillingApplyScheduledChangeArgs{ChangeId: a.ChangeID}}
	default:
		return setIngestArgs(req, args), nil
	}
	return true, nil
}

// SendEmailArgs converts a send_email task to its jobs.v1 message. The weekly
// report rides as JSON, which river-service passes back unchanged.
func SendEmailArgs(t SendEmailTask) (*jobsv1.SendEmailArgs, error) {
	m := &jobsv1.SendEmailArgs{
		UserId: t.UserID, TenantId: t.TenantID, TemplateKey: t.TemplateKey, EmailKind: string(t.EmailKind),
		IdempotencyKey: t.IdempotencyKey, ToEmail: t.ToEmail, ToName: t.ToName, Vars: t.Vars,
	}
	if t.Report != nil {
		report, err := json.Marshal(t.Report)
		if err != nil {
			return nil, fmt.Errorf("encode weekly report: %w", err)
		}
		m.Report = report
	}
	return m, nil
}

// SendEmailTaskFromArgs is SendEmailArgs reversed.
func SendEmailTaskFromArgs(a *jobsv1.SendEmailArgs) (SendEmailTask, error) {
	t := SendEmailTask{
		UserID: a.GetUserId(), TenantID: a.GetTenantId(), TemplateKey: a.GetTemplateKey(),
		EmailKind: models.EmailKind(a.GetEmailKind()), IdempotencyKey: a.GetIdempotencyKey(),
		ToEmail: a.GetToEmail(), ToName: a.GetToName(), Vars: a.GetVars(),
	}
	if report := a.GetReport(); len(report) > 0 {
		t.Report = &models.WeeklyReportView{}
		if err := json.Unmarshal(report, t.Report); err != nil {
			return SendEmailTask{}, fmt.Errorf("decode weekly report: %w", err)
		}
	}
	return t, nil
}

func setIngestArgs(req *jobsv1.EnqueueRequest, args JobArgs) bool {
	switch a := args.(type) {
	case ProcessPDFTask:
		req.Args = &jobsv1.EnqueueRequest_ProcessPdf{ProcessPdf: &jobsv1.ProcessPdfArgs{
			AssetId: a.AssetID, TenantId: a.TenantID, OriginalName: a.OriginalName, MimeType: a.MimeType,
		}}
	case ProcessDocumentTask:
		req.Args = &jobsv1.EnqueueRequest_ProcessDocument{ProcessDocument: &jobsv1.ProcessDocumentArgs{
			AssetId: a.AssetID, TenantId: a.TenantID, OriginalName: a.OriginalName, MimeType: a.MimeType, StorageKey: a.StorageKey,
		}}
	case ProcessURLTask:
		req.Args = &jobsv1.EnqueueRequest_ProcessUrl{ProcessUrl: &jobsv1.ProcessUrlArgs{
			AssetId: a.AssetID, TenantId: a.TenantID, SourceUrl: a.SourceURL, Refresh: a.Refresh,
		}}
	case ProcessImageTask:
		req.Args = &jobsv1.EnqueueRequest_ProcessImage{ProcessImage: &jobsv1.ProcessImageArgs{
			AssetId: a.AssetID, TenantId: a.TenantID, OriginalName: a.OriginalName, MimeType: a.MimeType,
			StorageKey: a.StorageKey, RunKey: a.RunKey, PinnedModel: a.PinnedModel,
		}}
	case ProcessAudioTask:
		req.Args = &jobsv1.EnqueueRequest_ProcessAudio{ProcessAudio: &jobsv1.ProcessAudioArgs{
			AssetId: a.AssetID, TenantId: a.TenantID, OriginalName: a.OriginalName, MimeType: a.MimeType,
			StorageKey: a.StorageKey, RunKey: a.RunKey, PinnedModel: a.PinnedModel,
		}}
	case ReembedImageTask:
		req.Args = &jobsv1.EnqueueRequest_ReembedImage{ReembedImage: &jobsv1.ReembedImageArgs{AssetId: a.AssetID, TenantId: a.TenantID}}
	case AnalyzeAssetTask:
		req.Args = &jobsv1.EnqueueRequest_AnalyzeAsset{AnalyzeAsset: &jobsv1.AnalyzeAssetArgs{
			TenantId: a.TenantID, AssetId: a.AssetID, CampaignId: a.CampaignID, Reason: a.Reason,
		}}
	case ReanalyzeCampaignTask:
		req.Args = &jobsv1.EnqueueRequest_ReanalyzeCampaign{ReanalyzeCampaign: &jobsv1.ReanalyzeCampaignArgs{TenantId: a.TenantID, CampaignId: a.CampaignID}}
	case DistillAssetIdeasTask:
		req.Args = &jobsv1.EnqueueRequest_DistillAssetIdeas{DistillAssetIdeas: &jobsv1.DistillAssetIdeasArgs{TenantId: a.TenantID, RunId: a.RunID}}
	case DistillBrandTask:
		req.Args = &jobsv1.EnqueueRequest_DistillBrand{DistillBrand: &jobsv1.DistillBrandArgs{TenantId: a.TenantID, RunId: a.RunID}}
	default:
		return false
	}
	return true
}
