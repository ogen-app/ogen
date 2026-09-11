//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/gif"
	"io"
	"net/http"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
)

// httpImagePreparer stands in for image-service in the minio-backed integration
// suite (CON-281). Unlike the unit fake, it does the real presigned-URL dance:
// GET the staged original, decode true dimensions, PUT the cleaned (pixel-
// identical) copy back to the destination key, and return metadata. A body that
// won't decode as any image is reported as Unimplemented so the handler answers
// 415 (unsupported media type), matching the pre-CON-281 imageprobe behaviour.
type httpImagePreparer struct{}

func (httpImagePreparer) PrepareAttachment(ctx context.Context, opts imageclient.PrepareAttachmentOptions) (*imageclient.PrepareAttachmentResult, error) {
	getResp, err := http.Get(opts.SourceURL)
	if err != nil {
		return nil, grpcstatus.Error(codes.Unavailable, err.Error())
	}
	raw, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()

	cfg, _, derr := image.DecodeConfig(bytes.NewReader(raw))
	if derr != nil {
		return nil, grpcstatus.Error(codes.Unimplemented, "not a supported image")
	}
	mime := http.DetectContentType(raw)
	animated := false
	if mime == "image/gif" {
		if g, gerr := gif.DecodeAll(bytes.NewReader(raw)); gerr == nil {
			animated = len(g.Image) > 1
		}
	}

	// PUT the cleaned copy (pixels preserved) to the destination key. The
	// Content-Type must match what the handler signed the URL with (the extension
	// MIME), which equals the sniffed MIME for the standard raster formats.
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, opts.DestPutURL, bytes.NewReader(raw))
	if err != nil {
		return nil, grpcstatus.Error(codes.Internal, err.Error())
	}
	putReq.Header.Set("Content-Type", mime)
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		return nil, grpcstatus.Error(codes.Unavailable, err.Error())
	}
	_ = putResp.Body.Close()

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

func (httpImagePreparer) GenerateAltText(_ context.Context, _ imageclient.GenerateAltTextOptions) (*imageclient.GenerateAltTextResult, error) {
	return &imageclient.GenerateAltTextResult{}, nil
}
