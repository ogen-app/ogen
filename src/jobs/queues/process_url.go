package queues

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
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

// ProcessURLQueue ingests a URL asset: scrape the page to Markdown via
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
	// Recorder + EmbedModel meter scrape + embedding usage. A
	// nil Recorder is a no-op. EmbedModel is the embed price-map key.
	Recorder   *usage.Recorder
	EmbedModel string
	// Notifier drops an in-app notification to the asset's creator when ingest
	// reaches a terminal status. Nil is a no-op.
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
	return river.InsertOpts{Queue: IngestQueue, MaxAttempts: 5}
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
	giveUp := func() error { return p.finish(ctx, in, models.AssetStatusFailed, "", 0, 0, 0, urlEmbedderUnavailable) }
	if ok, err := requireEmbedder(ctx, p.Deps.Embedder, "process_url", in.AssetID, lastAttempt, giveUp); !ok {
		return err
	}
	if err := p.statusWriter().set(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return err
	}
	p.publish(ctx, in, models.AssetStatusProcessing, "", 0, 0, 0, "")

	res, err := p.scrape(ctx, in)
	if err != nil {
		if firecrawl.IsTransient(err) {
			return fmt.Errorf("process_url %s: scrape: %w", in.AssetID, err)
		}
		slog.WarnContext(ctx, "unscrapeable url", logging.AttrComponent, "jobs.process_url", "asset_id", in.AssetID, logging.AttrError, err)
		return p.finish(ctx, in, models.AssetStatusFailed, "", 0, 0, 0, err.Error())
	}

	markdown, images, imgFailed := p.mirrorImages(ctx, in.AssetID, res.Markdown)
	title := cmp.Or(strings.TrimSpace(res.Title), in.SourceURL)
	if err := p.persistContent(ctx, in.AssetID, title, markdown, images); err != nil {
		return err
	}

	// The title is prepended for context, like embed_asset.
	chunks, stats := embedChunks(ctx, p.Deps.Embedder, in.AssetID, textChunkSources(flows.ChunkText(title+"\n\n"+markdown)))
	if err := storeChunks(ctx, p.Deps.Chunks, "process_url", in.AssetID, chunks, stats, true); err != nil {
		return err
	}

	final, err := stats.settle(lastAttempt)
	if err != nil {
		return fmt.Errorf("process_url %s: %w", in.AssetID, err)
	}
	switch {
	case final == models.AssetStatusFailed:
		return p.finish(ctx, in, final, title, len(images), 0, imgFailed, urlAllChunksFailed)
	case final == models.AssetStatusReady && imgFailed > 0:
		final = models.AssetStatusPartial
	}
	if err := p.finish(ctx, in, final, title, len(images), len(chunks), imgFailed, ""); err != nil {
		return err
	}
	// After the durable status write, so a retry from a late failure can't
	// double-count.
	if stats.Tokens > 0 {
		p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "url_embed", llm.EmbedUsage{Tokens: stats.Tokens})
	}
	return nil
}

// scrape fetches the page as Markdown and meters the call. On refresh it
// bypasses Firecrawl's cache.
func (p *ProcessURLProcessor) scrape(ctx context.Context, in ProcessURLTask) (*firecrawl.ScrapeResult, error) {
	req := firecrawl.ScrapeRequest{URL: in.SourceURL}
	if in.Refresh {
		req.MaxAge = new(0)
	}
	res, err := p.Deps.Scraper.Scrape(ctx, req)
	if err != nil {
		return nil, err
	}
	// Every successful scrape is a billable credit, so recording after a 200 is
	// accurate even across retries.
	p.Deps.Recorder.Record(ctx, firecrawl.VendorFirecrawl, "url_scrape", vendors.MeterEvent{
		Model:     "scrape",
		Operation: firecrawl.OpScrape,
		Usage:     vendors.Usage{vendors.KindURLScrape: 1},
	})
	return res, nil
}

// persistContent writes the title + rewritten Markdown, then replaces the
// mirrored images.
func (p *ProcessURLProcessor) persistContent(ctx context.Context, assetID, title, markdown string, images []models.AssetImage) error {
	if err := p.Deps.Assets.UpdateContent(ctx, assetID, title, markdown); err != nil {
		return fmt.Errorf("process_url %s: write content: %w", assetID, err)
	}
	if p.Deps.Images == nil {
		return nil
	}
	if err := p.Deps.Images.ReplaceForAsset(ctx, assetID, images); err != nil {
		return fmt.Errorf("process_url %s: store images: %w", assetID, err)
	}
	return nil
}

// textChunkSources yields plain text chunks indexed by position.
func textChunkSources(texts []string) iter.Seq[chunkSource] {
	return func(yield func(chunkSource) bool) {
		for i, text := range texts {
			if !yield(chunkSource{Index: i, Text: text}) {
				return
			}
		}
	}
}

// finish persists the terminal status and publishes the matching SSE frame. The
// status write error is propagated so the worker retries rather than reporting
// success with an unpersisted status.
func (p *ProcessURLProcessor) finish(ctx context.Context, in ProcessURLTask, status, title string, imageCount, chunkCount, failedImages int, errMsg string) error {
	w := p.statusWriter()
	var err error
	if status == models.AssetStatusFailed {
		err = w.fail(ctx, in.AssetID, urlFailureCode(errMsg), cmp.Or(errMsg, w.unavailableReason()))
	} else {
		err = w.set(ctx, in.AssetID, status)
	}
	if err != nil {
		return err
	}
	p.publish(ctx, in, status, title, imageCount, chunkCount, failedImages, errMsg)
	return nil
}

// urlFailureCode classifies a URL ingest failure: embedding problems are a
// service outage, anything else is a page that could not be scraped.
func urlFailureCode(errMsg string) string {
	switch errMsg {
	case urlEmbedderUnavailable, urlAllChunksFailed:
		return models.UploadCodeServiceUnavailable
	default:
		return models.UploadCodeInvalidFile
	}
}

const (
	urlEmbedderUnavailable = "embedder unavailable"
	urlAllChunksFailed     = "all chunks failed to embed"
)

func (p *ProcessURLProcessor) statusWriter() assetStatusWriter {
	return assetStatusWriter{op: "process_url", assets: p.Deps.Assets, notifier: p.Deps.Notifier, label: "link", kind: models.AssetTypeURL}
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
// imageMirrorParallelism caps how many images one URL asset fetches and
// re-hosts at once.
const imageMirrorParallelism = 4

// mirrorImage fetches one image and re-hosts it under storage key slot pos.
// ok is false on a failure; a nil img with ok true is a deliberate skip.
func (p *ProcessURLProcessor) mirrorImage(ctx context.Context, assetID string, pos int, url, alt string) (img *models.AssetImage, publicURL string, ok bool) {
	data, ct, err := p.fetcher().Fetch(ctx, url)
	if err != nil || !strings.HasPrefix(ct, "image/") {
		return nil, "", false
	}
	// SVGs can carry embedded scripts, so re-hosting an attacker-supplied one
	// under our storage origin is a stored-XSS vector. Never mirror them — the
	// original external link is kept (inert when the Markdown renders it as
	// an <img>). Not a failure: nothing broke, we deliberately skip it.
	if isSVGContentType(ct) {
		return nil, "", true
	}
	key := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/images/%d%s", assetID, pos, extForImage(ct, url)))
	publicURL, err = p.Deps.Storage.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), ct)
	if err != nil {
		return nil, "", false
	}
	id, err := models.NewID()
	if err != nil {
		return nil, "", false
	}
	img = &models.AssetImage{
		ID:        id,
		AssetID:   assetID,
		SourceURL: url,
		S3Key:     key,
		MimeType:  ct,
		SizeBytes: int64(len(data)),
	}
	if alt != "" {
		img.Alt = &alt
	}
	return img, publicURL, true
}

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

	// Images are fetched and re-hosted a few at a time; each result lands in
	// its ref's slot, so the order (and the idx numbering below) follows the
	// page regardless of which download finishes first.
	type mirrored struct {
		img       *models.AssetImage
		publicURL string
		failed    bool
	}
	results := make([]mirrored, len(refs))
	sem := make(chan struct{}, imageMirrorParallelism)
	var wg sync.WaitGroup
	for i, r := range refs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			img, publicURL, ok := p.mirrorImage(ctx, assetID, i, r.url, r.alt)
			results[i] = mirrored{img: img, publicURL: publicURL, failed: !ok}
		})
	}
	wg.Wait()

	var (
		images       []models.AssetImage
		replacements = make(map[string]string)
		failed       int
	)
	for i, res := range results {
		if res.failed {
			failed++
			continue
		}
		if res.img == nil {
			continue // deliberately skipped (SVG)
		}
		res.img.Idx = len(images)
		images = append(images, *res.img)
		replacements[refs[i].url] = res.publicURL
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
