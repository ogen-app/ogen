package handlers_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/gif"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
)

// fakeImagePreparer stands in for image-service in handler tests (CON-281). It
// resolves the presigned URLs back to stubStorage keys (the stub encodes the key
// in the URL), reads the staged original, decodes real dimensions, writes the
// "cleaned" (pixel-identical) copy to the destination key, and returns metadata —
// so the attachment light-path can be exercised without a live service. Alt-text
// generation returns empty so the async generator is a no-op in tests.
type fakeImagePreparer struct{ store *stubStorage }

func (f *fakeImagePreparer) PrepareAttachment(_ context.Context, opts imageclient.PrepareAttachmentOptions) (*imageclient.PrepareAttachmentResult, error) {
	srcKey := strings.TrimPrefix(opts.SourceURL, "https://pub.example.com/signed/")
	raw := f.store.objects[srcKey]
	if len(raw) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "empty image")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, "not a readable image")
	}
	// Write the cleaned (metadata-stripped, pixel-identical) copy to the dest key.
	destKey := strings.TrimPrefix(opts.DestPutURL, "https://pub.example.com/put/")
	if f.store.objects != nil {
		f.store.objects[destKey] = raw
	}
	mime := http.DetectContentType(raw)
	// Detect animated GIFs the way the real service does (frame count), so
	// platform validation for animated media still fires.
	animated := false
	if mime == "image/gif" {
		if g, gerr := gif.DecodeAll(bytes.NewReader(raw)); gerr == nil {
			animated = len(g.Image) > 1
		}
	}
	sum := sha256.Sum256(raw)
	return &imageclient.PrepareAttachmentResult{
		Mime:           mime,
		SizeBytes:      int64(len(raw)),
		Width:          cfg.Width,
		Height:         cfg.Height,
		IsAnimated:     animated,
		ChecksumSHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func (f *fakeImagePreparer) GenerateAltText(_ context.Context, _ imageclient.GenerateAltTextOptions) (*imageclient.GenerateAltTextResult, error) {
	// Empty alt → the async attachment generator writes nothing (keeps tests
	// deterministic; no post-response DB mutation to race with AfterEach).
	return &imageclient.GenerateAltTextResult{}, nil
}
