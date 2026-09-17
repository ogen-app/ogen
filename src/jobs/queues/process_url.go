package queues

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/genkit/flows"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/netguard"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// ProcessURLQueue ingests a URL asset (CON-222): scrape the page to Markdown via
// Firecrawl, mirror its images into object storage, persist content + images,
// then chunk + embed the Markdown — the same shape as process_pdf, with
// Firecrawl in place of pdf-service. Progress and the terminal result are
// published to the eventhub so the UI updates live over /api/events.
const ProcessURLQueue = "process_url"

const (
	// maxImagesPerAsset caps how many page images one scrape mirrors, bounding
	// the fan-out of image downloads + S3 writes.
	maxImagesPerAsset = 50
	// maxImageBytes is the per-image size ceiling; larger images are skipped
	// (the Markdown keeps the original external link).
	maxImageBytes = 10 << 20
	// assetEventType is the SSE discriminator for every URL-asset status change.
	assetEventType = "asset.updated"
)

// Narrow dependency interfaces — the real client/repos/storage satisfy these
// structurally; tests provide small fakes. chunkEmbedder is shared with
// process_pdf.go (same package).

type urlScraper interface {
	Scrape(ctx context.Context, in firecrawl.ScrapeRequest) (*firecrawl.ScrapeResult, error)
}

type assetContentWriter interface {
	UpdateContent(ctx context.Context, id, title, content string) error
	UpdateStatus(ctx context.Context, id, status string) error
	CreatorOf(ctx context.Context, id string) (string, error)
}

type imageReplacer interface {
	ReplaceForAsset(ctx context.Context, assetID string, images []models.AssetImage) error
}

type imageUploader interface {
	Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error)
}

type eventPublisher interface {
	Publish(ctx context.Context, ev eventhub.Event) error
}

// imageFetcher downloads a page image. Injected so tests can stub network I/O.
type imageFetcher interface {
	Fetch(ctx context.Context, rawURL string) (data []byte, contentType string, err error)
}

// URLDeps bundles the process_url worker's dependencies (built in server.go). A
// nil Scraper (no Firecrawl key path) makes the job a no-op.
type URLDeps struct {
	Scraper  urlScraper
	Embedder chunkEmbedder
	Storage  imageUploader
	Fetcher  imageFetcher
	Assets   assetContentWriter
	Chunks   chunkUpserter
	Images   imageReplacer
	Hub      eventPublisher
	// Recorder + EmbedModel meter scrape + embedding usage (CON-86/CON-222). A
	// nil Recorder is a no-op. EmbedModel is the embed price-map key.
	Recorder   *usage.Recorder
	EmbedModel string
	// Notifier drops an in-app notification to the asset's creator when ingest
	// reaches a terminal status (CON-242). Nil is a no-op.
	Notifier *notify.Service
}

// ProcessURLTask carries the asset to ingest. The scraped bytes are NOT in the
// args; the worker fetches them from Firecrawl on each attempt. Refresh flips
// Firecrawl's cache off so a re-submit re-fetches the live page.
type ProcessURLTask struct {
	AssetID   string `json:"asset_id"`
	TenantID  string `json:"tenant_id"`
	SourceURL string `json:"source_url"`
	Refresh   bool   `json:"refresh"`
}

func (ProcessURLTask) Kind() string { return ProcessURLQueue }

// InsertOpts bounds retries. Transient failures (network, 429/5xx, embedder
// outage) retry with backoff; terminal ones (4xx, unscrapeable page)
// short-circuit to "failed" inside Work.
func (ProcessURLTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

type ProcessURLProcessor struct {
	river.WorkerDefaults[ProcessURLTask]
	Deps URLDeps
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &ProcessURLProcessor{Deps: d.URL})
	})
}

// Work scopes the job to the asset's tenant and processes it. lastAttempt lets
// process() flip an otherwise-retryable outage to a terminal "failed" instead
// of abandoning the asset in "processing".
func (p *ProcessURLProcessor) Work(ctx context.Context, job *river.Job[ProcessURLTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.With(ctx, job.Args.TenantID)
	return p.process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

// Timeout covers the Firecrawl render, N image downloads, and per-chunk embedding.
func (p *ProcessURLProcessor) Timeout(*river.Job[ProcessURLTask]) time.Duration {
	return 10 * time.Minute
}

func (p *ProcessURLProcessor) process(ctx context.Context, in ProcessURLTask, lastAttempt bool) error {
	if p.Deps.Scraper == nil {
		slog.WarnContext(ctx, "firecrawl not configured", logging.AttrComponent, "jobs.process_url", "asset_id", in.AssetID)
		return nil
	}
	// No gemini_api_key yet (CON-104): fail fast rather than scrape + mirror only
	// to fail every embed. Retry — a key set via the secrets API takes effect
	// without a restart; give up only once attempts are exhausted.
	if !embedopts.Available(p.Deps.Embedder) {
		if lastAttempt {
			return p.finish(ctx, in, models.AssetStatusFailed, "", 0, 0, 0, "embedder unavailable")
		}
		slog.WarnContext(ctx, "embedder unavailable will retry", logging.AttrComponent, "jobs.process_url", "asset_id", in.AssetID)
		return fmt.Errorf("process_url %s: embedder unavailable", in.AssetID)
	}

	if err := p.setStatus(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return err
	}
	p.publish(ctx, in, models.AssetStatusProcessing, "", 0, 0, 0, "")

	// 1. Scrape → Markdown + metadata. On refresh, bypass Firecrawl's cache.
	req := firecrawl.ScrapeRequest{URL: in.SourceURL}
	if in.Refresh {
		zero := 0
		req.MaxAge = &zero
	}
	res, err := p.Deps.Scraper.Scrape(ctx, req)
	if err != nil {
		if firecrawl.IsTransient(err) {
			return fmt.Errorf("process_url %s: scrape: %w", in.AssetID, err)
		}
		slog.WarnContext(ctx, "unscrapeable url", logging.AttrComponent, "jobs.process_url", "asset_id", in.AssetID, logging.AttrError, err)
		return p.finish(ctx, in, models.AssetStatusFailed, "", 0, 0, 0, err.Error())
	}

	// CON-86/CON-222: one usage event per successful Firecrawl scrape (each call
	// is a real billable credit, so recording here — after a 200 — is accurate
	// even across retries). Nil recorder = no-op.
	p.Deps.Recorder.Record(ctx, firecrawl.VendorFirecrawl, "url_scrape", vendors.MeterEvent{
		Model:     "scrape",
		Operation: firecrawl.OpScrape,
		Usage:     vendors.Usage{vendors.KindURLScrape: 1},
	})

	// 2. Mirror images (best-effort) and rewrite the Markdown links.
	markdown, images, imgFailed := p.mirrorImages(ctx, in.AssetID, res.Markdown)

	title := strings.TrimSpace(res.Title)
	if title == "" {
		title = in.SourceURL
	}

	// 3. Persist content + title, then replace mirrored images.
	if err := p.Deps.Assets.UpdateContent(ctx, in.AssetID, title, markdown); err != nil {
		return fmt.Errorf("process_url %s: write content: %w", in.AssetID, err)
	}
	if p.Deps.Images != nil {
		if err := p.Deps.Images.ReplaceForAsset(ctx, in.AssetID, images); err != nil {
			return fmt.Errorf("process_url %s: store images: %w", in.AssetID, err)
		}
	}

	// 4. Chunk + embed the Markdown (title prepended for context, like embed_asset).
	fullText := title + "\n\n" + markdown
	chunkStrs := flows.ChunkText(fullText)
	chunks := make([]models.AssetChunk, 0, len(chunkStrs))
	var embedAttempts, embedFailures int
	var totalEmbedTokens int64
	for i, text := range chunkStrs {
		if !hasWords(text) {
			continue
		}
		embedAttempts++
		emb, eErr := p.Deps.Embedder.Embed(ctx, &ai.EmbedRequest{
			Input:   []*ai.Document{ai.DocumentFromText(text, nil)},
			Options: embedopts.Document(),
		})
		if eErr != nil || len(emb.Embeddings) != 1 {
			embedFailures++
			continue
		}
		tokens := estimateTokens(text)
		totalEmbedTokens += int64(tokens)
		chunks = append(chunks, models.AssetChunk{
			ID:         fmt.Sprintf("%s:%d", in.AssetID, i),
			AssetID:    in.AssetID,
			ChunkIndex: i,
			Content:    text,
			TokenCount: tokens,
			Embedding:  pgvector.NewHalfVector(emb.Embeddings[0].Embedding),
			Model:      p.Deps.Embedder.Name(),
		})
	}

	if p.Deps.Chunks != nil {
		if err := p.Deps.Chunks.UpsertChunks(ctx, in.AssetID, chunks); err != nil {
			return fmt.Errorf("process_url %s: store chunks: %w", in.AssetID, err)
		}
	}
	p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "url_embed", llm.EmbedUsage{Tokens: totalEmbedTokens})

	// 5. Final status.
	switch {
	case embedAttempts > 0 && len(chunks) == 0:
		// Every chunk failed to embed — almost always a transient embedder
		// outage. Retry; give up (failed) only once attempts are exhausted.
		if lastAttempt {
			return p.finish(ctx, in, models.AssetStatusFailed, title, len(images), 0, imgFailed, "all chunks failed to embed")
		}
		return fmt.Errorf("process_url %s: all %d chunk(s) failed to embed", in.AssetID, embedAttempts)
	case embedFailures > 0 || imgFailed > 0:
		return p.finish(ctx, in, models.AssetStatusPartial, title, len(images), len(chunks), imgFailed, "")
	default:
		return p.finish(ctx, in, models.AssetStatusReady, title, len(images), len(chunks), imgFailed, "")
	}
}

// finish persists the terminal status and publishes the matching SSE frame. The
// status write error is propagated so the worker retries rather than reporting
// success with an unpersisted status.
func (p *ProcessURLProcessor) finish(ctx context.Context, in ProcessURLTask, status, title string, imageCount, chunkCount, failedImages int, errMsg string) error {
	if err := p.setStatus(ctx, in.AssetID, status); err != nil {
		return err
	}
	p.publish(ctx, in, status, title, imageCount, chunkCount, failedImages, errMsg)
	return nil
}

// setStatus persists the asset status; a nil Assets dep is a no-op.
func (p *ProcessURLProcessor) setStatus(ctx context.Context, assetID, status string) error {
	if p.Deps.Assets == nil {
		return nil
	}
	if err := p.Deps.Assets.UpdateStatus(ctx, assetID, status); err != nil {
		return fmt.Errorf("process_url %s: set status %s: %w", assetID, status, err)
	}
	// CON-242: announce terminal outcomes to the asset's creator (no-op for the
	// intermediate "processing" write).
	notifyAssetStatus(ctx, p.Deps.Notifier, p.Deps.Assets, assetID, status, "link", models.AssetTypeURL)
	return nil
}

// assetEventPayload is the SSE `data:` body for an asset.updated frame.
type assetEventPayload struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	Type             string `json:"type"`
	Title            string `json:"title,omitempty"`
	ImageCount       int    `json:"image_count"`
	ChunkCount       int    `json:"chunk_count"`
	FailedImageCount int    `json:"failed_image_count"`
	Error            string `json:"error,omitempty"`
}

// publish fans an asset.updated event to the hub (topic entity:asset:<id>,
// tenant-scoped). Best-effort: a hub error never fails the job.
func (p *ProcessURLProcessor) publish(ctx context.Context, in ProcessURLTask, status, title string, imageCount, chunkCount, failedImages int, errMsg string) {
	if p.Deps.Hub == nil {
		return
	}
	_ = p.Deps.Hub.Publish(ctx, eventhub.Event{
		Topic:    "entity:asset:" + in.AssetID,
		TenantID: in.TenantID,
		Type:     assetEventType,
		Payload: assetEventPayload{
			ID:               in.AssetID,
			Status:           status,
			Type:             models.AssetTypeURL,
			Title:            title,
			ImageCount:       imageCount,
			ChunkCount:       chunkCount,
			FailedImageCount: failedImages,
			Error:            errMsg,
		},
	})
}

var (
	// mdImageRe matches a Markdown image: ![alt](url "optional title"), with the
	// url optionally wrapped in <>. Group 1 = alt, group 2 = url.
	mdImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*"|\s+'[^']*')?\s*\)`)
	// htmlImgRe matches an inline <img ... src="url" ...>. Group 1 = url.
	htmlImgRe = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)
)

// mirrorImages downloads each http(s) image referenced in the Markdown into
// tenant-scoped object storage and rewrites its links to the stored URL. It is
// best-effort: a failed image keeps its original external link and increments
// the returned failure count (which flips the asset to "partial"). With no
// storage configured it is a no-op that leaves the Markdown unchanged.
func (p *ProcessURLProcessor) mirrorImages(ctx context.Context, assetID, markdown string) (string, []models.AssetImage, int) {
	if p.Deps.Storage == nil {
		return markdown, nil, 0
	}

	type ref struct{ url, alt string }
	seen := make(map[string]bool)
	var refs []ref
	add := func(u, alt string) {
		if !isHTTPURL(u) || seen[u] {
			return
		}
		seen[u] = true
		refs = append(refs, ref{url: u, alt: alt})
	}
	for _, m := range mdImageRe.FindAllStringSubmatch(markdown, -1) {
		add(m[2], m[1])
	}
	for _, m := range htmlImgRe.FindAllStringSubmatch(markdown, -1) {
		add(m[1], "")
	}
	if len(refs) > maxImagesPerAsset {
		slog.WarnContext(ctx, "capping mirrored images", logging.AttrComponent, "jobs.process_url",
			"asset_id", assetID, "found", len(refs), "cap", maxImagesPerAsset)
		refs = refs[:maxImagesPerAsset]
	}

	var (
		images       []models.AssetImage
		replacements = make(map[string]string)
		failed       int
		idx          int
	)
	for _, r := range refs {
		data, ct, err := p.fetcher().Fetch(ctx, r.url)
		if err != nil || !strings.HasPrefix(ct, "image/") {
			failed++
			continue
		}
		// SVGs can carry embedded scripts, so re-hosting an attacker-supplied one
		// under our storage origin is a stored-XSS vector. Never mirror them — the
		// original external link is kept (inert when the Markdown renders it as
		// an <img>). Not a failure: nothing broke, we deliberately skip it.
		if isSVGContentType(ct) {
			continue
		}
		key := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/images/%d%s", assetID, idx, extForImage(ct, r.url)))
		publicURL, err := p.Deps.Storage.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), ct)
		if err != nil {
			failed++
			continue
		}
		id, err := models.NewID()
		if err != nil {
			failed++
			continue
		}
		img := models.AssetImage{
			ID:        id,
			AssetID:   assetID,
			Idx:       idx,
			SourceURL: r.url,
			S3Key:     key,
			MimeType:  ct,
			SizeBytes: int64(len(data)),
		}
		if r.alt != "" {
			alt := r.alt
			img.Alt = &alt
		}
		images = append(images, img)
		replacements[r.url] = publicURL
		idx++
	}

	// Apply replacements longest-key-first (deterministic) so a source URL that
	// is a prefix of another (e.g. ".../a.png" vs ".../a.png?w=100") can't clobber
	// the longer one before it is itself replaced.
	keys := make([]string, 0, len(replacements))
	for k := range replacements {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(len(b), len(a)); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	out := markdown
	for _, oldURL := range keys {
		out = strings.ReplaceAll(out, oldURL, replacements[oldURL])
	}
	return out, images, failed
}

func (p *ProcessURLProcessor) fetcher() imageFetcher {
	if p.Deps.Fetcher != nil {
		return p.Deps.Fetcher
	}
	return defaultImageFetcher
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// isSVGContentType reports whether ct is an SVG media type (with or without
// parameters, e.g. "image/svg+xml; charset=utf-8"). SVGs are never mirrored.
func isSVGContentType(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "image/svg")
}

// extForImage picks a file extension from the content type, falling back to the
// URL path's extension, then "". SVG is intentionally absent (SVGs are never
// mirrored — see isSVGContentType), and a ".svg" path fallback is rejected so no
// stored object is ever served as SVG.
func extForImage(contentType, rawURL string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/avif":
		return ".avif"
	}
	if u, err := url.Parse(rawURL); err == nil {
		if ext := strings.ToLower(path.Ext(u.Path)); ext != "" && ext != ".svg" && len(ext) <= 5 {
			return ext
		}
	}
	return ""
}

// defaultImageFetcher is the production image downloader. It uses an
// SSRF-guarded client (netguard.SafeClient) so a scraped page can't point us at
// internal/loopback/link-local addresses or cloud metadata — the dialer rejects
// blocked targets at connect time, closing the DNS-rebinding TOCTOU.
var defaultImageFetcher = &httpImageFetcher{client: netguard.SafeClient(30 * time.Second)}

type httpImageFetcher struct{ client *http.Client }

func (f *httpImageFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image fetch %s: status %d", rawURL, resp.StatusCode)
	}
	// Read one byte past the ceiling so an oversized image is detected and skipped.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("image %s exceeds %d bytes", rawURL, maxImageBytes)
	}
	return data, resp.Header.Get("Content-Type"), nil
}
