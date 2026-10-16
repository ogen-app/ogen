package remote_test

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	audiov1 "github.com/ogen-app/ogen/gen/audio/v1"
	pdfv1 "github.com/ogen-app/ogen/gen/pdf/v1"
	"github.com/ogen-app/ogen/src/transport/grpc/client/audio"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
	"github.com/ogen-app/ogen/src/transport/grpc/client/remote"
)

func TestPlanAndDecodeAUnaryCall(t *testing.T) {
	probe := func(out **audio.ProbeResult) func(grpc.ClientConnInterface) error {
		return func(cc grpc.ClientConnInterface) error {
			res, err := audio.Over(cc).Probe(context.Background(), audio.ProbeOptions{SourceURL: "https://s/x", Filename: "x.mp3"})
			if out != nil {
				*out = res
			}
			return err
		}
	}
	call, err := remote.Plan(probe(nil))
	if err != nil {
		t.Fatal(err)
	}
	var req audiov1.ProbeRequest
	if call.Method != "/audio.v1.AudioService/Probe" || call.Streamed || proto.Unmarshal(call.Request, &req) != nil || req.GetSourceUrl() != "https://s/x" {
		t.Fatalf("call = %+v (%v)", call, &req)
	}

	body, _ := proto.Marshal(&audiov1.ProbeResponse{DurationMs: 1234, Channels: 2})
	var got *audio.ProbeResult
	if err := remote.Decode(remote.Result{Body: body}, probe(&got)); err != nil || got.DurationMs != 1234 || got.Channels != 2 {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	err = remote.Decode(remote.Result{Code: int(codes.Unavailable), Message: "down"}, probe(nil))
	if err == nil {
		t.Error("a failed call decoded as success")
	}
}

func TestPlanAndDecodeAStreamedUpload(t *testing.T) {
	parse := func(out **pdf.Result) func(grpc.ClientConnInterface) error {
		return func(cc grpc.ClientConnInterface) error {
			res, err := pdf.Over(cc).Parse(context.Background(), bytes.NewReader(nil), pdf.Options{Filename: "a.pdf", RenderThumbnail: true})
			if out != nil {
				*out = res
			}
			return err
		}
	}
	call, err := remote.Plan(parse(nil))
	if err != nil {
		t.Fatal(err)
	}
	var first pdfv1.ParseRequest
	if call.Method != "/pdf.v1.PdfService/Parse" || !call.Streamed || proto.Unmarshal(call.Request, &first) != nil || first.GetOptions().GetFilename() != "a.pdf" {
		t.Fatalf("call = %+v", call)
	}

	body, _ := proto.Marshal(&pdfv1.ParseResponse{PageCount: 3, Chunks: []*pdfv1.Chunk{{Index: 0, Text: "hi", PageStart: 1, PageEnd: 1}}})
	var got *pdf.Result
	if err := remote.Decode(remote.Result{Body: body}, parse(&got)); err != nil || got.PageCount != 3 || len(got.Chunks) != 1 {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	err = remote.Decode(remote.Result{Code: int(codes.InvalidArgument), Message: "encrypted"}, parse(nil))
	if !pdf.IsInvalidPDF(err) {
		t.Errorf("InvalidArgument = %v, want the client's terminal verdict", err)
	}
}
