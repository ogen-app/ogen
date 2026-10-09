package zernio

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// commentRequest is the body for POST /inbox/comments/{postId}. Without a
// commentId the comment lands on the post itself.
type commentRequest struct {
	AccountID string `json:"accountId"`
	Message   string `json:"message"`
}

type commentEnvelope struct {
	Data struct {
		CommentID string `json:"commentId"`
	} `json:"data"`
}

// PostComment comments on a published post as accountID and returns the
// platform's id for the new comment. postID is the Zernio post id. Zernio
// replays the first response for a repeated idempotencyKey with the same body,
// so a retried call can't comment twice; while the first call is still in
// flight it answers 409, which IsTransientCommentError treats as retryable.
func (c *Client) PostComment(ctx context.Context, postID, accountID, message, idempotencyKey string) (string, error) {
	if c == nil {
		return "", errors.New("zernio: client is disabled")
	}
	if postID == "" || accountID == "" {
		return "", errors.New("zernio: postID and accountID are required")
	}
	var headers map[string]string
	if idempotencyKey != "" {
		headers = map[string]string{"Idempotency-Key": idempotencyKey}
	}
	var env commentEnvelope
	body := commentRequest{AccountID: accountID, Message: message}
	if err := c.doHeaders(ctx, http.MethodPost, "/inbox/comments/"+url.PathEscape(postID), nil, headers, body, &env); err != nil {
		return "", err
	}
	return env.Data.CommentID, nil
}

// IsTransientCommentError reports whether a PostComment error is worth
// retrying: anything IsTransientAPIError retries, plus 409, which on this
// endpoint means the same idempotency key is still being processed.
func IsTransientCommentError(err error) bool {
	return IsStatus(err, http.StatusConflict) || IsTransientAPIError(err)
}
