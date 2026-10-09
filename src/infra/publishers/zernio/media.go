package zernio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// MediaPresign is the response from POST /api/v1/media/presign. The
// bytes are PUT to UploadURL (one-hour validity), and PublicURL is referenced
// in a post's mediaItems. Uploads live in temporary storage for 7 days; Zernio
// copies them to permanent storage when a post referencing PublicURL publishes.
type MediaPresign struct {
	UploadURL string `json:"uploadUrl"`
	PublicURL string `json:"publicUrl"`
	Key       string `json:"key"`
	ExpiresIn int    `json:"expiresIn"`
}

type presignRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
}

// PresignMedia requests an upload URL + the permanent public URL for one media
// file (Zernio media-uploads guide). contentType is the file's MIME type.
func (c *Client) PresignMedia(ctx context.Context, filename, contentType string) (*MediaPresign, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	var out MediaPresign
	if err := c.do(ctx, http.MethodPost, "/media/presign", nil,
		presignRequest{Filename: filename, ContentType: contentType}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadMedia PUTs raw bytes to a presigned upload URL. Per the Zernio guide the
// upload URL is a direct storage URL that needs no auth, so this bypasses c.do
// (no API base URL, no Authorization header); only the Content-Type — which must
// match what PresignMedia was called with — is sent.
func (c *Client) UploadMedia(ctx context.Context, uploadURL, contentType string, body io.Reader, size int64) error {
	if c == nil {
		return errors.New("zernio: client is disabled")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if size > 0 {
		req.ContentLength = size
	}
	resp, err := c.uploadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return &APIError{Status: resp.StatusCode, Message: shortErrorMessage(raw)}
	}
	return nil
}

// MediaType maps a MIME type to Zernio's mediaItems `type` enum
// (image | video | document). Returns "" for a type Zernio does not accept, so
// the caller can skip that attachment rather than send an invalid item.
func MediaType(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case mime == "application/pdf":
		return "document"
	default:
		return ""
	}
}
