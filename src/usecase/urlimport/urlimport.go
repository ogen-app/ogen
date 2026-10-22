// Package urlimport turns a web page into a URL asset: it checks the URL, the
// scraping key and the import quotas, then creates the asset with its
// process_url job, or refreshes the asset already holding that URL.
package urlimport

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/netguard"
	"github.com/ogen-app/ogen/src/usecase/brandsource"
	"github.com/ogen-app/ogen/src/usecase/ingest"
)

// Quota features a new URL asset counts against.
const (
	FeatureWebPageImports = "web_page_imports"
	FeatureBankAssets     = "content_bank_assets"
)

// ErrUnavailable means URL import cannot run here: no job queue, database or
// scraping key.
var ErrUnavailable = errors.New("url scraping is not configured")

// InvalidURLError is a URL that cannot be imported. Its message is safe to
// show.
type InvalidURLError struct{ Msg string }

func (e *InvalidURLError) Error() string { return e.Msg }

// Enqueuer queues a URL's scrape inside the transaction that writes its
// asset. *queues.Enqueuer implements it.
type Enqueuer interface {
	EnqueueProcessURLTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, sourceURL string, refresh bool) error
}

// ScrapeGate reports whether a scraping key is configured. *firecrawl.Client
// implements it.
type ScrapeGate interface {
	HasKey(ctx context.Context) bool
}

// Service imports URLs. A nil Limiter gates nothing.
type Service struct {
	DB      *bun.DB
	Assets  repository.AssetRepository
	Ingest  *ingest.Service
	Jobs    Enqueuer
	Gate    ScrapeGate
	Limiter *entitlements.Limiter
}

// Input is one page to import. URL must come from Normalize.
type Input struct {
	TenantID string
	UserID   string
	URL      string
	// Mark stamps a new asset as a brand source; a refreshed one keeps its
	// own provenance.
	Mark *brandsource.Mark
}

// Result is the asset holding the page. Created is false when an existing
// asset with the URL was refreshed in place.
type Result struct {
	Asset   *models.Asset
	Created bool
}

// Configured reports whether an import can run: it needs the job queue, the
// database (the asset and its job are written together) and a scraping key.
func (s *Service) Configured(ctx context.Context) bool {
	return s != nil && s.Jobs != nil && s.DB != nil && s.Gate != nil && s.Gate.HasKey(ctx)
}

// Import creates the URL asset and queues its scrape, or refreshes the asset
// already holding the URL. A new asset counts against web_page_imports and
// content_bank_assets; a refresh counts against nothing. Quota refusals are
// the limiter's own errors.
func (s *Service) Import(ctx context.Context, in Input) (*Result, error) {
	if !s.Configured(ctx) {
		return nil, ErrUnavailable
	}
	existing, err := s.Assets.GetBySourceURL(ctx, in.URL)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if existing != nil {
		return s.refresh(ctx, existing, in)
	}
	// The URL sub-cap is checked first, so a workspace with bank room left but
	// out of URL imports is refused on the allowance that ran out.
	imports, err := s.Limiter.Require(ctx, in.TenantID, FeatureWebPageImports)
	if err != nil {
		return nil, err
	}
	bank, err := s.Limiter.Require(ctx, in.TenantID, FeatureBankAssets)
	if err != nil {
		return nil, err
	}
	asset, err := newURLAsset(in)
	if err != nil {
		return nil, err
	}
	if _, err := s.Ingest.Create(ctx, ingest.NewAsset{
		Asset: asset,
		Enqueue: func(ctx context.Context, tx *sql.Tx) error {
			return s.Jobs.EnqueueProcessURLTx(ctx, tx, asset.ID, in.TenantID, in.URL, false)
		},
	}); err != nil {
		// A concurrent submit of the same new URL loses on the partial unique
		// index; refresh the winner so the call stays idempotent.
		if again, gErr := s.Assets.GetBySourceURL(ctx, in.URL); gErr == nil && again != nil {
			return s.refresh(ctx, again, in)
		}
		return nil, err
	}
	s.Limiter.DispatchCrossing(ctx, in.TenantID, imports)
	s.Limiter.DispatchCrossing(ctx, in.TenantID, bank)
	return &Result{Asset: asset, Created: true}, nil
}

func newURLAsset(in Input) (*models.Asset, error) {
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	typ := models.AssetTypeURL
	a := &models.Asset{
		ID: id,
		// Provisional; the worker sets the page title.
		Title:     in.URL,
		Status:    models.AssetStatusPending,
		Type:      &typ,
		SourceURL: new(in.URL),
		TagIDs:    models.StringSlice{},
		Tags:      []models.Tag{},
		CreatedBy: in.UserID,
	}
	if in.Mark != nil {
		in.Mark.Apply(a)
	}
	return a, nil
}

// refresh resets an existing URL asset to pending and queues a scrape that
// bypasses the scraper's cache. The worker replaces content, images and
// chunks; id, tags and provenance stay.
func (s *Service) refresh(ctx context.Context, asset *models.Asset, in Input) (*Result, error) {
	if err := s.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().
			Model((*models.Asset)(nil)).
			Set("status = ?", models.AssetStatusPending).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", asset.ID).
			Exec(ctx); err != nil {
			return err
		}
		return s.Jobs.EnqueueProcessURLTx(ctx, tx.Tx, asset.ID, in.TenantID, in.URL, true)
	}); err != nil {
		return nil, err
	}
	asset.Status = models.AssetStatusPending
	return &Result{Asset: asset}, nil
}

// Normalize checks and canonicalises a submitted URL, then runs the SSRF
// pre-flight: a host resolving to a private, loopback, link-local or metadata
// address is refused. The worker's fetcher is the authoritative connect-time
// guard; this one fails early. The lookup is bounded so a slow resolver
// cannot stall the caller.
func Normalize(ctx context.Context, raw string) (string, error) {
	normalized, host, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := netguard.ResolveAllowed(lookupCtx, host); err != nil {
		return "", &InvalidURLError{Msg: "url host is not allowed"}
	}
	return normalized, nil
}

// Canonical validates and canonicalises a URL for dedupe without resolving
// it: http(s) only, lower-cased scheme and host, no fragment, no trailing
// slash (the query is kept). It returns the URL and its host.
func Canonical(raw string) (normalized, host string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", &InvalidURLError{Msg: "invalid url"}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", &InvalidURLError{Msg: "only http(s) urls are supported"}
	}
	if u.Host == "" {
		return "", "", &InvalidURLError{Msg: "url is missing a host"}
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path != "/" {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	return u.String(), netguard.NormalizeHost(u.Hostname()), nil
}
