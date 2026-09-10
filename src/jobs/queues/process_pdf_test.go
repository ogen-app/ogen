package queues

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
)

// ---- fakes (satisfy the job's narrow dependency interfaces) ----

type fakeParser struct {
	res      *pdf.Result
	err      error
	gotOpts  pdf.Options
	gotBytes []byte
}

func (f *fakeParser) Parse(_ context.Context, r io.Reader, opts pdf.Options) (*pdf.Result, error) {
	f.gotOpts = opts
	f.gotBytes, _ = io.ReadAll(r)
	return f.res, f.err
}

// fakeEmbedder returns one 4-dim embedding per input. failAll fails every call;
// failCalls fails specific 1-based call indices (to model some-fail-some-pass).
type fakeEmbedder struct {
	failAll   bool
	failCalls map[int]bool
	calls     int
}

func (f *fakeEmbedder) Name() string { return "fake-embedder" }

func (f *fakeEmbedder) Embed(_ context.Context, req *ai.EmbedRequest) (*ai.EmbedResponse, error) {
	f.calls++
	if f.failAll || f.failCalls[f.calls] {
		return nil, errors.New("embed: unavailable")
	}
	embs := make([]*ai.Embedding, 0, len(req.Input))
	for range req.Input {
		embs = append(embs, &ai.Embedding{Embedding: []float32{0.1, 0.2, 0.3, 0.4}})
	}
	return &ai.EmbedResponse{Embeddings: embs}, nil
}

type fakeBlob struct {
	data        []byte
	downloadErr error
	uploads     map[string][]byte
}

func (f *fakeBlob) Download(_ context.Context, _ string) (io.ReadCloser, error) {
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (f *fakeBlob) Upload(_ context.Context, key string, r io.Reader, _ int64, _ string) (string, error) {
	b, _ := io.ReadAll(r)
	if f.uploads == nil {
		f.uploads = map[string][]byte{}
	}
	f.uploads[key] = b
	return "https://stub/" + key, nil
}

type fakeStatus struct {
	all    []string
	failOn string // when set, UpdateStatus fails for this status value
}

func (f *fakeStatus) UpdateStatus(_ context.Context, _, status string) error {
	f.all = append(f.all, status)
	if f.failOn != "" && status == f.failOn {
		return errors.New("status: db down")
	}
	return nil
}
func (f *fakeStatus) CreatorOf(context.Context, string) (string, error) { return "", nil }
func (f *fakeStatus) last() string {
	if len(f.all) == 0 {
		return ""
	}
	return f.all[len(f.all)-1]
}

type fakeChunks struct {
	got   []models.AssetChunk
	calls int
}

func (f *fakeChunks) UpsertChunks(_ context.Context, _ string, chunks []models.AssetChunk) error {
	f.got = chunks
	f.calls++
	return nil
}

type fakeFiles struct {
	got *models.AssetFile
	err error // when set, Upsert fails
}

func (f *fakeFiles) Upsert(_ context.Context, file *models.AssetFile) error {
	f.got = file
	return f.err
}

func newProc(d PDFDeps) *ProcessPDFProcessor { return &ProcessPDFProcessor{Deps: d} }

func TestProcessPDF_Success(t *testing.T) {
	parser := &fakeParser{res: &pdf.Result{
		PageCount:    2,
		Chunks:       []pdf.Chunk{{Index: 0, Text: "hello world", PageStart: 1, PageEnd: 1}},
		ThumbnailPNG: []byte("PNGDATA"),
	}}
	blob := &fakeBlob{data: []byte("%PDF-1.7 body")}
	status := &fakeStatus{}
	chunks := &fakeChunks{}
	files := &fakeFiles{}
	p := newProc(PDFDeps{Client: parser, Embedder: &fakeEmbedder{}, Storage: blob, Assets: status, Chunks: chunks, Files: files})

	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a1", OriginalName: "x.pdf", MimeType: "application/pdf"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	if !bytes.Equal(parser.gotBytes, blob.data) {
		t.Fatalf("parser received %q, want the downloaded pdf", parser.gotBytes)
	}
	if !parser.gotOpts.RenderThumbnail || parser.gotOpts.Filename != "x.pdf" || parser.gotOpts.ThumbnailDPI != thumbnailDPIDefault {
		t.Fatalf("parse options not propagated: %+v", parser.gotOpts)
	}
	if len(chunks.got) != 1 || chunks.got[0].ID != "a1:0" || chunks.got[0].Content != "hello world" ||
		chunks.got[0].PageStart == nil || *chunks.got[0].PageStart != 1 {
		t.Fatalf("unexpected stored chunk: %+v", chunks.got)
	}
	var thumb string
	for k := range blob.uploads {
		if strings.HasSuffix(k, "thumbnail.png") {
			thumb = k
		}
	}
	if thumb == "" {
		t.Fatalf("thumbnail not uploaded; uploads=%v", blob.uploads)
	}
	if files.got == nil || files.got.PageCount == nil || *files.got.PageCount != 2 ||
		files.got.ThumbnailS3Key == nil || files.got.OriginalName != "x.pdf" {
		t.Fatalf("asset file not persisted as expected: %+v", files.got)
	}
	if status.last() != models.AssetStatusReady {
		t.Fatalf("final status = %q, want ready (saw %v)", status.last(), status.all)
	}
}

func TestProcessPDF_FinalStatusWriteFailurePropagates(t *testing.T) {
	parser := &fakeParser{res: &pdf.Result{
		PageCount: 1,
		Chunks:    []pdf.Chunk{{Index: 0, Text: "hello world", PageStart: 1, PageEnd: 1}},
	}}
	// "processing" succeeds; the terminal "ready" write fails.
	status := &fakeStatus{failOn: models.AssetStatusReady}
	p := newProc(PDFDeps{Client: parser, Embedder: &fakeEmbedder{},
		Storage: &fakeBlob{data: []byte("pdf")}, Assets: status, Chunks: &fakeChunks{}, Files: &fakeFiles{}})

	// The job must fail (so River retries) rather than report success with the
	// asset stranded in "processing".
	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a8"}, false); err == nil {
		t.Fatal("expected non-nil error when the final status write fails")
	}
}

func TestProcessPDF_FileUpsertFailurePropagates(t *testing.T) {
	parser := &fakeParser{res: &pdf.Result{
		PageCount: 1,
		Chunks:    []pdf.Chunk{{Index: 0, Text: "hello world", PageStart: 1, PageEnd: 1}},
	}}
	status := &fakeStatus{}
	files := &fakeFiles{err: errors.New("files: db down")}
	p := newProc(PDFDeps{Client: parser, Embedder: &fakeEmbedder{},
		Storage: &fakeBlob{data: []byte("pdf")}, Assets: status, Chunks: &fakeChunks{}, Files: files})

	// A failed asset_file upsert must fail the job (so River retries) and must
	// not let the asset be marked ready without its file row.
	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a9"}, false); err == nil {
		t.Fatal("expected non-nil error when the asset_file upsert fails")
	}
	if status.last() == models.AssetStatusReady {
		t.Fatalf("asset must not be ready when the file row failed to persist; saw %v", status.all)
	}
}

func TestProcessPDF_PartialOnSomeEmbedFailures(t *testing.T) {
	parser := &fakeParser{res: &pdf.Result{PageCount: 1, Chunks: []pdf.Chunk{
		{Index: 0, Text: "good chunk"},
		{Index: 1, Text: "bad chunk"},
	}}}
	status := &fakeStatus{}
	chunks := &fakeChunks{}
	p := newProc(PDFDeps{Client: parser, Embedder: &fakeEmbedder{failCalls: map[int]bool{2: true}},
		Storage: &fakeBlob{data: []byte("pdf")}, Assets: status, Chunks: chunks, Files: &fakeFiles{}})

	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a2"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if status.last() != models.AssetStatusPartial {
		t.Fatalf("status = %q, want partial", status.last())
	}
	if len(chunks.got) != 1 {
		t.Fatalf("expected 1 successfully embedded chunk stored, got %d", len(chunks.got))
	}
	// The stored chunk had zero-value (unknown) page bounds — they must stay nil,
	// not be persisted as an invalid page 0.
	if chunks.got[0].PageStart != nil || chunks.got[0].PageEnd != nil {
		t.Fatalf("unknown page bounds should be nil, got start=%v end=%v",
			chunks.got[0].PageStart, chunks.got[0].PageEnd)
	}
}

func TestProcessPDF_AllEmbedsFail_RetriesThenFailsOnLastAttempt(t *testing.T) {
	mk := func() (*ProcessPDFProcessor, *fakeStatus) {
		st := &fakeStatus{}
		return newProc(PDFDeps{
			Client:   &fakeParser{res: &pdf.Result{Chunks: []pdf.Chunk{{Index: 0, Text: "text"}}}},
			Embedder: &fakeEmbedder{failAll: true},
			Storage:  &fakeBlob{data: []byte("pdf")},
			Assets:   st, Chunks: &fakeChunks{}, Files: &fakeFiles{},
		}), st
	}

	// Not the last attempt: surface a retryable error, do NOT mark failed.
	p, st := mk()
	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a3"}, false); err == nil {
		t.Fatal("expected a retryable error when every chunk fails to embed")
	}
	for _, s := range st.all {
		if s == models.AssetStatusFailed {
			t.Fatal("must not mark failed before the last attempt")
		}
	}

	// Last attempt: give up cleanly (failed, nil error) so the asset isn't stuck.
	p2, st2 := mk()
	if err := p2.process(t.Context(), ProcessPDFTask{AssetID: "a3"}, true); err != nil {
		t.Fatalf("last attempt should not return an error: %v", err)
	}
	if st2.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", st2.last())
	}
}

func TestProcessPDF_TerminalParseErrorFailsNoRetry(t *testing.T) {
	status := &fakeStatus{}
	p := newProc(PDFDeps{
		Client:   &fakeParser{err: grpcstatus.Error(codes.InvalidArgument, "corrupt pdf")},
		Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("pdf")},
		Assets: status, Chunks: &fakeChunks{}, Files: &fakeFiles{},
	})
	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a4"}, false); err != nil {
		t.Fatalf("terminal parse error must not be retried (want nil err): %v", err)
	}
	if status.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", status.last())
	}
}

func TestProcessPDF_TransientParseErrorRetries(t *testing.T) {
	p := newProc(PDFDeps{
		Client:   &fakeParser{err: grpcstatus.Error(codes.Unavailable, "service down")},
		Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("pdf")},
		Assets: &fakeStatus{}, Chunks: &fakeChunks{}, Files: &fakeFiles{},
	})
	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a5"}, false); err == nil {
		t.Fatal("transient parse error should be retried (want non-nil err)")
	}
}

func TestProcessPDF_EmptyPDFReadyNoChunks(t *testing.T) {
	status := &fakeStatus{}
	chunks := &fakeChunks{}
	p := newProc(PDFDeps{
		Client:   &fakeParser{res: &pdf.Result{PageCount: 1}},
		Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("pdf")},
		Assets: status, Chunks: chunks, Files: &fakeFiles{},
	})
	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a6"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if status.last() != models.AssetStatusReady {
		t.Fatalf("status = %q, want ready", status.last())
	}
	if chunks.calls != 0 {
		t.Fatalf("no chunks should be upserted for an empty pdf")
	}
}

func TestProcessPDF_DisabledClientNoOp(t *testing.T) {
	status := &fakeStatus{}
	p := newProc(PDFDeps{Client: nil, Assets: status})
	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a7"}, false); err != nil {
		t.Fatalf("disabled client should no-op: %v", err)
	}
	if len(status.all) != 0 {
		t.Fatalf("disabled client must not touch status, saw %v", status.all)
	}
}
