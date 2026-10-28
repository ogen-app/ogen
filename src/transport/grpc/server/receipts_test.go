package server

import (
	"context"
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	receiptsv1 "github.com/ogen-app/ogen/gen/receipts/v1"
	"github.com/ogen-app/ogen/src/genkit/flows/receipt_extraction"
	"github.com/ogen-app/ogen/src/infra/email/resend"
	"github.com/ogen-app/ogen/src/usecase/receipts"
)

type stubFetcher struct{ raw []byte }

func (s stubFetcher) GetReceivedRaw(_ context.Context, id string, _ int64) ([]byte, error) {
	if id == "gone" {
		return nil, resend.ErrNotFound
	}
	return s.raw, nil
}

type stubExtractor struct{}

func (stubExtractor) ExtractReceipt(context.Context, receipt_extraction.Request) (*receipt_extraction.Response, error) {
	total, line := int64(20_000_000), int64(5_000_000)
	return &receipt_extraction.Response{
		Extraction: receipt_extraction.Extraction{
			Kind: receipt_extraction.KindInvoice, MerchantName: "Cloudflare, Inc.", Currency: "USD", TotalMicros: &total,
			LineItems: []receipt_extraction.LineItem{{Description: "Domain renewal", AmountMicros: &line}},
		},
		Usage: receipt_extraction.Usage{ModelID: "claude-haiku-5-5", PromptVersion: "receipt-v1", InputTokens: 3000, OutputTokens: 400, CostMicros: 5000},
	}, nil
}

// bigReceipt is a forwarded invoice whose PDF puts the email over gRPC's
// default 4 MiB message limit.
func bigReceipt() []byte {
	pdf := base64.StdEncoding.EncodeToString(append([]byte("%PDF-1.4 "), make([]byte, 5<<20)...))
	var b strings.Builder
	b.WriteString("From: ops@getogen.com\r\nTo: receipts@in.getogen.com\r\nSubject: Fwd: Invoice\r\nMessage-ID: <m-1@x>\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/html\r\n\r\n<p>Total $20.00</p>\r\n")
	b.WriteString("--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=invoice.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	for len(pdf) > 76 {
		b.WriteString(pdf[:76] + "\r\n")
		pdf = pdf[76:]
	}
	b.WriteString(pdf + "\r\n--b--\r\n")
	return []byte(b.String())
}

func dialReceipts(t *testing.T, p ReceiptProcessor) receiptsv1.ReceiptServiceClient {
	t.Helper()
	const token = "receipts-token"
	srv, err := New(Deps{Token: token, Secrets: newFakeStore(), Receipts: p})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(receiptsMaxMsgBytes), grpc.MaxCallSendMsgSize(receiptsMaxMsgBytes)),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), method, req, reply, cc, opts...)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return receiptsv1.NewReceiptServiceClient(conn)
}

func TestProcessReceiptRoundTrip(t *testing.T) {
	raw := bigReceipt()
	cli := dialReceipts(t, &receipts.Service{Fetcher: stubFetcher{raw: raw}, Extractor: stubExtractor{}})
	ctx := t.Context()

	for name, req := range map[string]*receiptsv1.ProcessReceiptRequest{
		"resend id": {Source: &receiptsv1.ProcessReceiptRequest_ResendEmailId{ResendEmailId: "em-1"}},
		"raw mime":  {Source: &receiptsv1.ProcessReceiptRequest_RawMime{RawMime: raw}},
	} {
		res, err := cli.ProcessReceipt(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(res.GetRawMime()) != len(raw) || res.GetHeaders().GetMessageId() != "m-1@x" || res.GetHtmlBody() != "<p>Total $20.00</p>" {
			t.Fatalf("%s: email = %s / %v", name, res.GetHeaders(), len(res.GetRawMime()))
		}
		if a := res.GetAttachments(); len(a) != 1 || a[0].GetFilename() != "invoice.pdf" || len(a[0].GetContent()) != 9+5<<20 {
			t.Fatalf("%s: attachments = %d", name, len(a))
		}
		e := res.GetExtraction()
		if e.GetKind() != receiptsv1.ReceiptKind_RECEIPT_KIND_INVOICE || e.GetTotalMicros() != 20_000_000 || e.SubtotalMicros != nil ||
			len(e.GetLineItems()) != 1 || e.GetLineItems()[0].GetAmountMicros() != 5_000_000 || e.GetLineItems()[0].QuantityMicros != nil {
			t.Fatalf("%s: extraction = %v", name, e)
		}
		if u := res.GetUsage(); u.GetModelId() != "claude-haiku-5-5" || u.GetPromptVersion() != "receipt-v1" || u.GetCostMicros() != 5000 {
			t.Fatalf("%s: usage = %v", name, u)
		}
	}

	codesFor := map[codes.Code]*receiptsv1.ProcessReceiptRequest{
		codes.NotFound:        {Source: &receiptsv1.ProcessReceiptRequest_ResendEmailId{ResendEmailId: "gone"}},
		codes.InvalidArgument: {Source: &receiptsv1.ProcessReceiptRequest_RawMime{RawMime: []byte("garbage")}},
	}
	for want, req := range codesFor {
		if _, err := cli.ProcessReceipt(ctx, req); status.Code(err) != want {
			t.Errorf("code = %v, want %v", status.Code(err), want)
		}
	}
	if _, err := cli.ProcessReceipt(ctx, &receiptsv1.ProcessReceiptRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no source: %v", err)
	}
}

func TestProcessReceiptOffWithoutProcessor(t *testing.T) {
	cli := dialReceipts(t, nil)
	_, err := cli.ProcessReceipt(t.Context(), &receiptsv1.ProcessReceiptRequest{Source: &receiptsv1.ProcessReceiptRequest_ResendEmailId{ResendEmailId: "em-1"}})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented", err)
	}
}
