package queues

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	pdfv1 "github.com/ogen-app/ogen/gen/pdf/v1"
	"github.com/ogen-app/ogen/src/domain/models"
)

// driveSteps runs a job's steps the way river-service does: no local
// closures, answer gives each action's raw result.
func driveSteps(t *testing.T, step stepFunc, kind string, args any, attempt, maxAttempts int, answer func(StepOutput) *StepResult) StepOutput {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	in := StepInput{Kind: kind, Args: raw, Attempt: attempt, MaxAttempts: maxAttempts}
	for range 100 {
		out, err := step(t.Context(), in)
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		if out.Done != nil || out.Retry != nil {
			return out
		}
		in.State, in.Result = out.State, answer(out)
	}
	t.Fatal("job did not finish")
	return StepOutput{}
}

// geminiEmbeds answers a planned batchEmbedContents call with one vector per
// text, failing any request that holds one of fail.
func geminiEmbeds(t *testing.T, c *HTTPCall, fail ...string) *StepResult {
	t.Helper()
	if c.Upstream != UpstreamGemini || !strings.HasSuffix(c.Path, ":batchEmbedContents") {
		t.Fatalf("unexpected call %s %s", c.Upstream, c.Path)
	}
	var body struct {
		Requests []struct {
			Content struct {
				Parts []struct{ Text string } `json:"parts"`
			} `json:"content"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(c.Body, &body); err != nil {
		t.Fatal(err)
	}
	var embs []string
	for _, r := range body.Requests {
		if slices.Contains(fail, r.Content.Parts[0].Text) {
			return &StepResult{Status: 400, Body: []byte(`{"error":{"message":"rejected"}}`)}
		}
		embs = append(embs, `{"values":[0.1,0.2,0.3,0.4]}`)
	}
	return &StepResult{Status: 200, Body: fmt.Appendf(nil, `{"embeddings":[%s]}`, strings.Join(embs, ","))}
}

func TestProcessPDFRunsInRiverService(t *testing.T) {
	status, chunks, files := &fakeStatus{}, &fakeChunks{}, &fakeFiles{}
	p := newProc(PDFDeps{Client: &fakeParser{}, Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("%PDF-1.7 body")},
		Assets: status, Chunks: chunks, Files: files})
	parsed, _ := proto.Marshal(&pdfv1.ParseResponse{PageCount: 2, ThumbnailPng: []byte("PNG"), Chunks: []*pdfv1.Chunk{
		{Index: 0, Text: "good chunk", PageStart: 1, PageEnd: 1}, {Index: 1, Text: "bad chunk", PageStart: 2, PageEnd: 2},
	}})
	out := driveSteps(t, p.step, ProcessPDFQueue, ProcessPDFTask{AssetID: "a1", OriginalName: "x.pdf"}, 1, 5, func(o StepOutput) *StepResult {
		switch {
		case o.Sidecar != nil:
			sc := o.Sidecar
			if sc.Service != sidecarPDF || sc.Method != "/pdf.v1.PdfService/Parse" || sc.Stream == nil || sc.Stream.ChunkField != 2 || len(sc.Stream.Header) == 0 {
				t.Fatalf("sidecar call = %+v", sc)
			}
			return &StepResult{Status: int(codes.OK), Body: parsed}
		case o.HTTP != nil:
			return geminiEmbeds(t, o.HTTP, "bad chunk")
		}
		t.Fatalf("unexpected action %+v", o)
		return nil
	})
	if out.Done == nil || status.last() != models.AssetStatusPartial {
		t.Fatalf("out %+v, status %v", out, status.all)
	}
	if len(chunks.got) != 1 || chunks.got[0].Content != "good chunk" || chunks.got[0].Model != "fake-embedder" {
		t.Fatalf("chunks = %+v", chunks.got)
	}
	if files.got == nil || *files.got.PageCount != 2 || files.got.SizeBytes != int64(len("%PDF-1.7 body")) || files.got.ThumbnailS3Key == nil {
		t.Fatalf("file = %+v", files.got)
	}
}

func TestProcessPDFRemoteParseRejection(t *testing.T) {
	status := &fakeStatus{}
	p := newProc(PDFDeps{Client: &fakeParser{}, Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("pdf")},
		Assets: status, Chunks: &fakeChunks{}, Files: &fakeFiles{}})
	answer := func(code codes.Code) func(StepOutput) *StepResult {
		return func(StepOutput) *StepResult { return &StepResult{Status: int(code), Err: "nope"} }
	}
	if out := driveSteps(t, p.step, ProcessPDFQueue, ProcessPDFTask{AssetID: "a2"}, 1, 5, answer(codes.InvalidArgument)); out.Done == nil || status.failCode != models.UploadCodeInvalidFile {
		t.Errorf("invalid pdf: out %+v, code %q", out, status.failCode)
	}
	if out := driveSteps(t, p.step, ProcessPDFQueue, ProcessPDFTask{AssetID: "a3"}, 1, 5, answer(codes.Unavailable)); out.Retry == nil {
		t.Errorf("service down: out %+v, want a retry", out)
	}
}
