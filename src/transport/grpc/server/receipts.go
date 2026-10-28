package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	receiptsv1 "github.com/ogen-app/ogen/gen/receipts/v1"
	"github.com/ogen-app/ogen/src/genkit/flows/receipt_extraction"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/receipts"
)

// receiptsMaxMsgBytes is the gRPC message limit the receipt service needs:
// a response carries the raw email (up to 25 MiB) and its decoded
// attachments, and a re-extraction request carries the raw email.
const receiptsMaxMsgBytes = 64 << 20

// ReceiptProcessor is the receipts usecase Harbor drives.
// *receipts.Service satisfies it.
type ReceiptProcessor interface {
	Process(ctx context.Context, resendEmailID string, raw []byte) (*receipts.Result, error)
}

// registerReceipts wires receipts.v1 ReceiptService. With no processor
// (nil) the service answers Unimplemented.
func registerReceipts(srv *grpc.Server, p ReceiptProcessor) {
	if p == nil {
		return
	}
	receiptsv1.RegisterReceiptServiceServer(srv, &receiptService{p: p})
}

type receiptService struct {
	receiptsv1.UnimplementedReceiptServiceServer
	p ReceiptProcessor
}

func (s *receiptService) ProcessReceipt(ctx context.Context, req *receiptsv1.ProcessReceiptRequest) (*receiptsv1.ProcessReceiptResponse, error) {
	var (
		id  string
		raw []byte
	)
	switch src := req.GetSource().(type) {
	case *receiptsv1.ProcessReceiptRequest_ResendEmailId:
		id = strings.TrimSpace(src.ResendEmailId)
		if id == "" {
			return nil, status.Error(codes.InvalidArgument, "resend_email_id is empty")
		}
	case *receiptsv1.ProcessReceiptRequest_RawMime:
		raw = src.RawMime
		if len(raw) == 0 {
			return nil, status.Error(codes.InvalidArgument, "raw_mime is empty")
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "one of resend_email_id or raw_mime is required")
	}
	res, err := s.p.Process(ctx, id, raw)
	if err != nil {
		return nil, receiptStatusErr(ctx, err)
	}
	return toReceiptProto(res), nil
}

func receiptStatusErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, receipts.ErrNotFound):
		return status.Error(codes.NotFound, "Resend no longer has the email")
	case errors.Is(err, receipts.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, receipts.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		slog.WarnContext(ctx, "process receipt unavailable", logging.AttrComponent, "grpcserver", logging.AttrError, err)
		return status.Error(codes.Unavailable, "receipt processing is unavailable; retry later")
	default:
		slog.ErrorContext(ctx, "process receipt failed", logging.AttrComponent, "grpcserver", logging.AttrError, err)
		return status.Error(codes.Internal, "receipt processing failed")
	}
}

var receiptKinds = map[string]receiptsv1.ReceiptKind{
	receipt_extraction.KindInvoice:    receiptsv1.ReceiptKind_RECEIPT_KIND_INVOICE,
	receipt_extraction.KindReceipt:    receiptsv1.ReceiptKind_RECEIPT_KIND_RECEIPT,
	receipt_extraction.KindPrepayment: receiptsv1.ReceiptKind_RECEIPT_KIND_PREPAYMENT,
	receipt_extraction.KindRefund:     receiptsv1.ReceiptKind_RECEIPT_KIND_REFUND,
	receipt_extraction.KindCreditNote: receiptsv1.ReceiptKind_RECEIPT_KIND_CREDIT_NOTE,
}

func toReceiptProto(r *receipts.Result) *receiptsv1.ProcessReceiptResponse {
	m := r.Message
	headers := &receiptsv1.EmailHeaders{From: m.From, To: m.To, Subject: m.Subject, MessageId: m.MessageID, ForwardedFrom: m.ForwardedFrom}
	if m.Date != nil {
		headers.Date = timestamppb.New(*m.Date)
	}
	out := &receiptsv1.ProcessReceiptResponse{
		RawMime: r.Raw, Headers: headers, TextBody: m.TextBody, HtmlBody: m.HTMLBody,
		Usage: &receiptsv1.ModelUsage{
			ModelId: r.Usage.ModelID, PromptVersion: r.Usage.PromptVersion,
			InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, CostMicros: r.Usage.CostMicros,
		},
	}
	for _, a := range m.Attachments {
		out.Attachments = append(out.Attachments, &receiptsv1.Attachment{Filename: a.Filename, ContentType: a.ContentType, Content: a.Content})
	}
	e := r.Extraction
	ex := &receiptsv1.Extraction{
		Kind: receiptKinds[e.Kind], MerchantName: e.MerchantName, DocumentNumber: e.DocumentNumber,
		IssueDate: e.IssueDate, ServicePeriodStart: e.ServicePeriodStart, ServicePeriodEnd: e.ServicePeriodEnd,
		Currency: e.Currency, SubtotalMicros: e.SubtotalMicros, TaxMicros: e.TaxMicros, TotalMicros: e.TotalMicros,
		ConfidenceNotes: e.ConfidenceNotes,
	}
	for _, l := range e.LineItems {
		ex.LineItems = append(ex.LineItems, &receiptsv1.LineItem{
			Description: l.Description, QuantityMicros: l.QuantityMicros, UnitPriceMicros: l.UnitPriceMicros, AmountMicros: l.AmountMicros,
		})
	}
	out.Extraction = ex
	return out
}
