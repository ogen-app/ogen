package assistantkit

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// User-facing messages for a turn that failed before the assistant could
// answer. The raw error is for logs and Sentry only: it can carry provider
// payloads, tool internals and ids the user has no use for.
const (
	MsgBusy      = "The AI service is busy right now. Please try again in a moment."
	MsgTimeout   = "That took too long to finish. Please try again, or ask for something smaller."
	MsgCancelled = "The request was cancelled."
	MsgDeclined  = "I can't help with that request. Try rephrasing it."
	MsgGeneric   = "Something went wrong on my side. Please try again."
)

// FriendlyError maps a failed model call to a message the user can act on.
func FriendlyError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return MsgCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return MsgTimeout
	case errors.Is(err, llm.ErrRefused):
		return MsgDeclined
	case busy(err):
		return MsgBusy
	}
	return MsgGeneric
}

// busy reports a provider rate limit or overload. The genkit plugin does not
// always keep the SDK error in the chain, so the provider's own wording is
// checked too — but not in a tool's error, whose text (a count of 429 posts)
// says nothing about the provider.
func busy(err error) bool {
	if apiErr, ok := errors.AsType[*anthropic.Error](err); ok {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable, 529:
			return true
		}
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "error calling tool") || (strings.Contains(msg, `tool "`) && strings.Contains(msg, `" failed:`)) {
		return false
	}
	return strings.Contains(msg, "overloaded_error") || strings.Contains(msg, "rate_limit_error") ||
		strings.Contains(msg, "429 too many requests") || strings.Contains(msg, "529 ")
}
