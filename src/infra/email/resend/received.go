package resend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrTooLarge is a received email bigger than the caller's limit.
var ErrTooLarge = errors.New("resend: received email too large")

// GetReceivedRaw downloads a received (inbound) email as raw RFC 5322 bytes:
// it reads the email (GET /emails/receiving/{id}) for its signed raw-download
// link, then fetches that link. It returns ErrDisabled without a key,
// ErrNotFound when Resend no longer has the email, and ErrTooLarge past
// maxBytes; any other error is worth a retry.
func (c *Client) GetReceivedRaw(ctx context.Context, id string, maxBytes int64) ([]byte, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: empty email id", ErrNotFound)
	}
	key, err := c.keyResolver(ctx)
	if err != nil {
		return nil, fmt.Errorf("resend: resolve api key: %w", err)
	}
	if key == "" {
		return nil, ErrDisabled
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/emails/receiving/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, shortErrorMessage(raw))
		}
		return nil, fmt.Errorf("resend: get received email: status %d: %s", resp.StatusCode, shortErrorMessage(raw))
	}
	var out struct {
		Raw struct {
			DownloadURL string `json:"download_url"`
		} `json:"raw"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("resend: decode received email: %w", err)
	}
	if out.Raw.DownloadURL == "" {
		return nil, fmt.Errorf("resend: received email %s has no raw download link", id)
	}
	return c.download(ctx, out.Raw.DownloadURL, maxBytes)
}

// download fetches the signed raw link. The link carries its own signature,
// so no key is sent with it.
func (c *Client) download(ctx context.Context, link string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && !strings.HasPrefix(c.baseURL, "http://")) {
		return nil, fmt.Errorf("resend: unexpected raw download link")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	// The link is fetched under the caller's deadline, not the API timeout:
	// a large email may take longer than an API call.
	resp, err := (&http.Client{Transport: c.httpClient.Transport}).Do(req)
	if err != nil {
		// The link carries its signature; keep it out of the error text.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, fmt.Errorf("resend: download received email: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: raw download", ErrNotFound)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("resend: download received email: status %d", resp.StatusCode)
	case resp.ContentLength > maxBytes:
		return nil, ErrTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("resend: download received email: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, ErrTooLarge
	}
	return raw, nil
}
