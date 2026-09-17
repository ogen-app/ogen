package queues

import (
	"context"
	"database/sql"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/ogen-app/ogen/src/domain/models"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
)

// --- image-specific fakes (presignBlob / fakeEmbedder / fakeChunks are shared) ---

type fakeImageClient struct {
	res     *imageclient.ExtractResult
	err     error
	gotOpts imageclient.ExtractOptions
}

func (f *fakeImageClient) Extract(_ context.Context, opts imageclient.ExtractOptions) (*imageclient.ExtractResult, error) {
	f.gotOpts = opts
	return f.res, f.err
}

type fakeImageAssets struct {
	statuses []string
	content  string
	alt      string
	setAlt   bool
	wrote    bool
}

func (f *fakeImageAssets) UpdateStatus(_ context.Context, _, status string) error {
	f.statuses = append(f.statuses, status)
	return nil
}
func (f *fakeImageAssets) CreatorOf(context.Context, string) (string, error) { return "", nil }
func (f *fakeImageAssets) GetByID(_ context.Context, id string) (*models.Asset, error) {
	return &models.Asset{ID: id, Content: f.content}, nil
}
func (f *fakeImageAssets) SetImageResult(_ context.Context, _, content, altText string, setAlt bool) error {
	f.wrote = true
	f.content = content
	f.alt = altText
	f.setAlt = setAlt
	return nil
}
func (f *fakeImageAssets) last() string {
	if len(f.statuses) == 0 {
		return ""
	}
	return f.statuses[len(f.statuses)-1]
}

type fakeImageFiles struct{ file *models.AssetFile }

func (f *fakeImageFiles) GetByAssetID(context.Context, string) (*models.AssetFile, error) {
	if f.file == nil {
		return &models.AssetFile{}, nil
	}
	return f.file, nil
}
func (f *fakeImageFiles) Upsert(_ context.Context, file *models.AssetFile) error {
	f.file = file
	return nil
}

type fakeImageExtractions struct {
	ext    *models.ImageExtraction
	create int
	update int
}

func (f *fakeImageExtractions) Create(_ context.Context, e *models.ImageExtraction) error {
	f.create++
	f.ext = e
	return nil
}
func (f *fakeImageExtractions) GetByAssetAndRunKey(_ context.Context, _, _ string) (*models.ImageExtraction, error) {
	if f.ext == nil {
		return nil, sql.ErrNoRows
	}
	return f.ext, nil
}
func (f *fakeImageExtractions) Update(_ context.Context, e *models.ImageExtraction) error {
	f.update++
	f.ext = e
	return nil
}

type fakeImageBlocks struct {
	got   []models.ImageBlock
	calls int
}

func (f *fakeImageBlocks) ReplaceForExtraction(_ context.Context, _ string, blocks []models.ImageBlock) error {
	f.calls++
	f.got = blocks
	return nil
}
func (f *fakeImageBlocks) ListByExtraction(_ context.Context, _ string) ([]models.ImageBlock, error) {
	return f.got, nil
}

func newImageProc(d ImageDeps) *ProcessImageProcessor { return &ProcessImageProcessor{Deps: d} }

func baseImageDeps(client imageExtractor) (ImageDeps, *fakeImageAssets, *fakeChunks, *fakeImageBlocks, *fakeImageExtractions) {
	assets := &fakeImageAssets{}
	chunks := &fakeChunks{}
	blocks := &fakeImageBlocks{}
	exts := &fakeImageExtractions{}
	return ImageDeps{
		Client:        client,
		Embedder:      &fakeEmbedder{},
		Storage:       presignBlob{},
		Assets:        assets,
		Chunks:        chunks,
		Files:         &fakeImageFiles{},
		Extractions:   exts,
		Blocks:        blocks,
		ClassifyModel: "gemini-2.5-flash",
		ExtractModel:  "gemini-2.5-pro",
		EscalateModel: "gemini-2.5-pro",
	}, assets, chunks, blocks, exts
}

// TestProcessImage_Success is the heart of the job: a successful Extract persists
// the extraction (complete) + blocks (with image-region anchors), sets the asset
// description + alt text, embeds description + block text into chunks, and snapshots
// a non-zero cost from the gemini price table.
func TestProcessImage_Success(t *testing.T) {
	res := &imageclient.ExtractResult{
		Shape:              models.ImageShapeProse,
		ClassifyConfidence: 0.92,
		Description:        "A screenshot of a login form.",
		AltText:            "Login form screenshot",
		DescriptionOK:      true,
		ExtractionOK:       true,
		Normalized:         imageclient.NormalizedMeta{Mime: "image/png", Width: 800, Height: 600, ChecksumSHA256: "norm-sum"},
		Blocks: []imageclient.Block{{
			Kind:       "paragraph",
			Text:       "Username Password Sign in",
			Provenance: "image_extraction",
			Anchor:     imageclient.Anchor{Kind: "image", Bbox: &imageclient.Bbox{X: 0.1, Y: 0.2, W: 0.5, H: 0.3}},
		}},
		Usage: []imageclient.TokenUsage{{Model: "gemini-2.5-pro", Step: "vision_extract", Input: 1000, Output: 200}},
	}
	deps, assets, chunks, blocks, exts := baseImageDeps(&fakeImageClient{res: res})
	p := newImageProc(deps)

	task := ProcessImageTask{AssetID: "i1", StorageKey: "assets/i1/original.png", RunKey: "run-1", MimeType: "image/png"}
	if err := p.process(t.Context(), task, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	if exts.ext == nil || exts.ext.Status != models.ImageExtractionStatusComplete {
		t.Fatalf("extraction status = %v, want complete", exts.ext)
	}
	if exts.ext.Shape != models.ImageShapeProse || exts.ext.CostMicros <= 0 || exts.ext.PriceVersion == "" {
		t.Fatalf("extraction metadata wrong: %+v", exts.ext)
	}
	if blocks.calls != 1 || len(blocks.got) != 1 {
		t.Fatalf("want 1 block persisted, got calls=%d n=%d", blocks.calls, len(blocks.got))
	}
	b := blocks.got[0]
	if b.Anchor == nil || b.Anchor.Kind != "image" || b.Anchor.Bbox == nil || b.Anchor.Bbox.W != 0.5 {
		t.Fatalf("block anchor wrong: %+v", b.Anchor)
	}
	if !assets.wrote || assets.content != res.Description || assets.alt != res.AltText || !assets.setAlt {
		t.Fatalf("asset description/alt not written: %+v", assets)
	}
	// stampFile records the browser-drawable normalized key so decorateFile can
	// mint normalized_url (CON-299), and — since images arrive with no thumbnail —
	// fills the empty thumbnail slot with the same key so the preview cell draws a
	// picture. No tenant in ctx, so the key is unprefixed. Dimensions are stamped too.
	ff := deps.Files.(*fakeImageFiles)
	const wantKey = "assets/i1/normalized.png"
	if ff.file == nil {
		t.Fatal("asset_file was not stamped")
	}
	if ff.file.NormalizedS3Key == nil || *ff.file.NormalizedS3Key != wantKey {
		t.Fatalf("normalized_s3_key = %v, want %q", ff.file.NormalizedS3Key, wantKey)
	}
	if ff.file.ThumbnailS3Key == nil || *ff.file.ThumbnailS3Key != wantKey {
		t.Fatalf("thumbnail_s3_key = %v, want %q (empty slot should be filled)", ff.file.ThumbnailS3Key, wantKey)
	}
	if ff.file.Width != 800 || ff.file.Height != 600 {
		t.Fatalf("dimensions not stamped: %dx%d", ff.file.Width, ff.file.Height)
	}
	// description + one block text = two embedded chunks.
	if len(chunks.got) != 2 {
		t.Fatalf("want 2 chunks (description + block), got %d", len(chunks.got))
	}
	if chunks.got[0].SourceAnchor == nil || chunks.got[0].SourceAnchor.Kind != "image" {
		t.Fatalf("description chunk should carry an image anchor: %+v", chunks.got[0].SourceAnchor)
	}
	if assets.last() != models.AssetStatusReady {
		t.Fatalf("final status = %q, want ready (saw %v)", assets.last(), assets.statuses)
	}
}

// TestExposeNormalizedDerivative_KeepsExistingThumbnail: the thumbnail slot is
// filled only when empty. A file that already has one — a real downscaled
// thumbnail, or a PDF's first-page preview — keeps it, while the normalized key is
// still recorded so normalized_url is available (CON-299).
func TestExposeNormalizedDerivative_KeepsExistingThumbnail(t *testing.T) {
	existing := "assets/i9/thumb.png"
	files := &fakeImageFiles{file: &models.AssetFile{ThumbnailS3Key: &existing}}
	p := newImageProc(ImageDeps{Files: files})
	if err := p.exposeNormalizedDerivative(t.Context(), ProcessImageTask{AssetID: "i9"}); err != nil {
		t.Fatalf("exposeNormalizedDerivative: %v", err)
	}
	if files.file.ThumbnailS3Key == nil || *files.file.ThumbnailS3Key != existing {
		t.Fatalf("thumbnail clobbered: %v, want %q", files.file.ThumbnailS3Key, existing)
	}
	if files.file.NormalizedS3Key == nil || *files.file.NormalizedS3Key != "assets/i9/normalized.png" {
		t.Fatalf("normalized_s3_key = %v, want the normalized key", files.file.NormalizedS3Key)
	}
}

// TestProcessImage_ExtractionFailedIsPartial: a description-ok / extraction-failed
// run stays searchable (description embedded) but settles to partial.
func TestProcessImage_ExtractionFailedIsPartial(t *testing.T) {
	res := &imageclient.ExtractResult{
		Shape:         models.ImageShapeTabular,
		Description:   "A table screenshot.",
		AltText:       "table",
		DescriptionOK: true,
		ExtractionOK:  false,
		Normalized:    imageclient.NormalizedMeta{Mime: "image/png"},
	}
	deps, assets, _, _, exts := baseImageDeps(&fakeImageClient{res: res})
	if err := newImageProc(deps).process(t.Context(), ProcessImageTask{AssetID: "i2", StorageKey: "assets/i2/original.png", RunKey: "run-1"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if assets.last() != models.AssetStatusPartial {
		t.Fatalf("status = %q, want partial", assets.last())
	}
	if exts.ext.Status != models.ImageExtractionStatusPartial {
		t.Fatalf("extraction status = %q, want partial", exts.ext.Status)
	}
	// A partial run carries the machine-readable code so the client can word it
	// as searchable-but-incomplete rather than broken (CON-281).
	if exts.ext.FailureCode != models.UploadCodeExtractionPartial {
		t.Fatalf("failure_code = %q, want %q", exts.ext.FailureCode, models.UploadCodeExtractionPartial)
	}
}

// TestProcessImage_UnsupportedIsTerminal: a terminal service verdict marks the
// asset failed and is NOT retried (nil error), stamping a machine-readable code.
func TestProcessImage_UnsupportedIsTerminal(t *testing.T) {
	deps, assets, _, _, exts := baseImageDeps(&fakeImageClient{err: grpcstatus.Error(codes.Unimplemented, "svg not supported")})
	if err := newImageProc(deps).process(t.Context(), ProcessImageTask{AssetID: "i3", StorageKey: "assets/i3/original.svg", RunKey: "run-1"}, false); err != nil {
		t.Fatalf("terminal reject must not be retried (want nil err): %v", err)
	}
	if assets.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", assets.last())
	}
	// Unimplemented == "format not supported" → the unsupported-media code.
	if exts.ext.FailureCode != models.UploadCodeUnsupportedMediaType {
		t.Fatalf("failure_code = %q, want %q", exts.ext.FailureCode, models.UploadCodeUnsupportedMediaType)
	}
}

// TestProcessImage_FineRejectCode: a terminal reject carrying an image.v1
// ErrorInfo reason maps to the FINE failure_code, refining the coarse gRPC-code
// bucket (CON-281 Phase 2). InvalidArgument alone would settle to invalid_file;
// the REJECTED_CODE_VECTOR detail upgrades it to vector_rejected.
func TestProcessImage_FineRejectCode(t *testing.T) {
	st, derr := grpcstatus.New(codes.InvalidArgument, "vector images are not supported").
		WithDetails(&errdetails.ErrorInfo{Domain: "image.v1", Reason: "REJECTED_CODE_VECTOR"})
	if derr != nil {
		t.Fatalf("build status detail: %v", derr)
	}
	deps, assets, _, _, exts := baseImageDeps(&fakeImageClient{err: st.Err()})
	if err := newImageProc(deps).process(t.Context(), ProcessImageTask{AssetID: "i6", StorageKey: "assets/i6/original.svg", RunKey: "run-1"}, false); err != nil {
		t.Fatalf("terminal reject must not be retried (want nil err): %v", err)
	}
	if assets.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", assets.last())
	}
	if exts.ext.FailureCode != models.UploadCodeVectorRejected {
		t.Fatalf("failure_code = %q, want %q (fine reason should beat the coarse bucket)", exts.ext.FailureCode, models.UploadCodeVectorRejected)
	}
}

// TestProcessImage_EmbedFailureLastAttemptSettles: a total embed failure retries
// on earlier attempts, but on the FINAL attempt it settles the asset terminally
// instead of stranding it in `processing` / the extraction in `describing` once
// River gives up (CodeRabbit #149).
func TestProcessImage_EmbedFailureLastAttemptSettles(t *testing.T) {
	res := &imageclient.ExtractResult{
		Shape:         models.ImageShapeProse,
		Description:   "A screenshot.",
		AltText:       "shot",
		DescriptionOK: true,
		ExtractionOK:  true,
		Normalized:    imageclient.NormalizedMeta{Mime: "image/png"},
	}

	// Not the last attempt: a total embed outage is retryable (non-nil error).
	deps, _, _, _, _ := baseImageDeps(&fakeImageClient{res: res})
	deps.Embedder = &fakeEmbedder{failAll: true}
	if err := newImageProc(deps).process(t.Context(), ProcessImageTask{AssetID: "e1", StorageKey: "assets/e1/original.png", RunKey: "run-1"}, false); err == nil {
		t.Fatal("total embed failure should retry (want non-nil err) when not last attempt")
	}

	// Final attempt: settle terminally, no error — the asset must not strand.
	deps2, assets2, _, _, exts2 := baseImageDeps(&fakeImageClient{res: res})
	deps2.Embedder = &fakeEmbedder{failAll: true}
	if err := newImageProc(deps2).process(t.Context(), ProcessImageTask{AssetID: "e2", StorageKey: "assets/e2/original.png", RunKey: "run-1"}, true); err != nil {
		t.Fatalf("last attempt must settle (want nil err): %v", err)
	}
	if assets2.last() != models.AssetStatusFailed {
		t.Fatalf("asset status = %q, want failed (not stranded in processing)", assets2.last())
	}
	if exts2.ext.Status != models.ImageExtractionStatusFailed {
		t.Fatalf("extraction status = %q, want failed", exts2.ext.Status)
	}
	// AC4: a failed run must NOT publish a normalized_url. Dimensions are stamped
	// pre-checkpoint (stampFile), but the browser-drawable key is exposed only at
	// searchable settlement — which this run never reached.
	if ff := deps2.Files.(*fakeImageFiles).file; ff != nil && ff.NormalizedS3Key != nil {
		t.Fatalf("failed extraction must not expose a normalized key (AC4), got %q", *ff.NormalizedS3Key)
	}
}

// TestProcessImage_TransientRetries: a transient service error is returned so
// River retries; the asset is not marked failed.
func TestProcessImage_TransientRetries(t *testing.T) {
	deps, _, _, _, _ := baseImageDeps(&fakeImageClient{err: grpcstatus.Error(codes.Unavailable, "service down")})
	if err := newImageProc(deps).process(t.Context(), ProcessImageTask{AssetID: "i4", StorageKey: "assets/i4/original.png", RunKey: "run-1"}, false); err == nil {
		t.Fatal("transient error should be retried (want non-nil err)")
	}
}

// TestProcessImage_DisabledClientNoOp: a nil client (service unwired) no-ops
// without touching the asset.
func TestProcessImage_DisabledClientNoOp(t *testing.T) {
	assets := &fakeImageAssets{}
	p := newImageProc(ImageDeps{Client: nil, Assets: assets})
	if err := p.process(t.Context(), ProcessImageTask{AssetID: "i5"}, false); err != nil {
		t.Fatalf("disabled client should no-op: %v", err)
	}
	if len(assets.statuses) != 0 {
		t.Fatalf("disabled client must not touch status, saw %v", assets.statuses)
	}
}
