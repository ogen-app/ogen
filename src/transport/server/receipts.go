package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"

	"github.com/ogen-app/ogen/src/genkit/flows/receipt_extraction"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// ReceiptExtractor runs the receipt_extraction flow for the operator gRPC
// surface. It owns a Genkit instance of its own, rebuilt when the Anthropic
// key changes, because the gRPC server is not part of the HTTP app that owns
// the tenant flows.
type ReceiptExtractor struct {
	mu sync.RWMutex
	fn func(ctx context.Context, req receipt_extraction.Request) (*receipt_extraction.Response, error)
}

// NewReceiptExtractor builds the extractor and keeps it current with the
// anthropic_api_key secret.
func NewReceiptExtractor(ctx context.Context, store secrets.Store) (*ReceiptExtractor, error) {
	// A receipt's PDF goes to the model as a document block.
	InstallAnthropicPDFDocuments()
	r := &ReceiptExtractor{}
	if err := r.rebuild(ctx, store); err != nil {
		return nil, err
	}
	store.Subscribe(secrets.NameAnthropicAPIKey, func() {
		if err := r.rebuild(context.Background(), store); err != nil {
			slog.Error("receipt extraction rebuild after anthropic_api_key change failed", logging.AttrComponent, "genkit", logging.AttrError, err)
		}
	})
	return r, nil
}

func (r *ReceiptExtractor) rebuild(ctx context.Context, store secrets.Store) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, err := store.Get(ctx, secrets.NameAnthropicAPIKey)
	if errors.Is(err, secrets.ErrNotFound) || (err == nil && key == "") {
		r.fn = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("receipt extraction: read anthropic_api_key: %w", err)
	}
	g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{APIKey: key}))
	fn, err := receipt_extraction.Init(g, receipt_extraction.FlowConfig{Provider: llm.NewProvider()})
	if err != nil {
		return fmt.Errorf("init receipt extraction flow: %w", err)
	}
	r.fn = fn
	return nil
}

// ExtractReceipt runs the flow, or returns ErrAnthropicUnavailable while no
// key is set.
func (r *ReceiptExtractor) ExtractReceipt(ctx context.Context, req receipt_extraction.Request) (*receipt_extraction.Response, error) {
	r.mu.RLock()
	fn := r.fn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, req)
}
