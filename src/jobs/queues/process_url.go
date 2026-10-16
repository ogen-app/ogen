package queues

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/netguard"
	"github.com/ogen-app/ogen/src/kernel/pubdate"
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
	// Analysis queues the asset's analyses once it settles; nil skips them.
	Analysis AnalysisScheduler
	Scraper  urlScraper
	Embedder chunkEmbedder
	Storage  imageUploader
	Fetcher  imageFetcher
	Assets   assetContentWriter
	// Published records the page's published time; nil skips it.
	Published publishedAtSetter
	Chunks    chunkUpserter
	Images    imageReplacer
	Hub       eventPublisher
	// Recorder + EmbedModel meter scrape + embedding usage. A
	// nil Recorder is a no-op. EmbedModel is the embed price-map key.
	Recorder   *usage.Recorder
	EmbedModel string
	// Notifier drops an in-app notification to the asset's creator when ingest
	// reaches a terminal status. Nil is a no-op.
	Notifier *notify.Service
	// Stages holds the page's chunks while they embed. Nil keeps them in
	// memory, which serves only a run inside one process.
	Stages chunkStage
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

type ProcessURLProcessor struct {
	Deps URLDeps

	memOnce sync.Once
	mem     *memChunkStage
}

// process runs one attempt in-process. lastAttempt flips an otherwise
// retryable outage to a terminal "failed" instead of abandoning the asset in
// "processing".
func (p *ProcessURLProcessor) process(ctx context.Context, in ProcessURLTask, lastAttempt bool) error {
	attempt, maxAttempts := 1, 2
	if lastAttempt {
		attempt = 2
	}
	return runLocal(ctx, p.step, ProcessURLQueue, in, attempt, maxAttempts, localActions{})
}

func (p *ProcessURLProcessor) stages() chunkStage {
	if p.Deps.Stages != nil {
		return p.Deps.Stages
	}
	p.memOnce.Do(func() { p.mem = newMemChunkStage(p.Deps.Chunks) })
	return p.mem
}

func (p *ProcessURLProcessor) embedLoop() embedLoop {
	return embedLoop{stage: p.stages(), embedder: p.Deps.Embedder}
}

// urlState carries a run between steps: the scraped page while its images
// are mirrored one by one, then how far embedding got.
type urlState struct {
	Phase string `json:"phase"`
	Title string `json:"title,omitempty"`
	// Markdown is the scraped page, gzipped: it rides along while the
	// images are mirrored.
	Markdown     []byte              `json:"markdown,omitempty"`
	Refs         []imageRef          `json:"refs,omitempty"`
	Next         int                 `json:"next,omitzero"`
	Images       []models.AssetImage `json:"images,omitempty"`
	Replacements map[string]string   `json:"replacements,omitempty"`
	FailedImages int                 `json:"failed_images,omitzero"`
	Embed        embedProgress       `json:"embed"`
	// Empty marks a run that staged no chunks: the page has nothing
	// embeddable, so its earlier chunks are cleared at commit.
	Empty bool `json:"empty,omitzero"`
}

// imageRef is one page image to mirror.
type imageRef struct {
	URL string `json:"url"`
	Alt string `json:"alt,omitempty"`
}

const (
	urlPhaseScrape = "scrape"
	urlPhaseImages = "images"
	urlPhaseEmbed  = "embed"
)

// step scrapes the page (Firecrawl), mirrors its images into storage one at a
// time, persists the content, then stages and embeds its chunks.
func (p *ProcessURLProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task ProcessURLTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	ctx = withTaskTenant(ctx, task.TenantID)
	var st urlState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	switch st.Phase {
	case "":
		return p.start(ctx, task, in.lastAttempt())
	case urlPhaseScrape:
		return p.scraped(ctx, task, in.Result)
	case urlPhaseImages:
		p.fetched(ctx, task, &st, in.Result)
		return p.nextImage(ctx, task, &st)
	case urlPhaseEmbed:
		if err := p.embedLoop().settle(ctx, &st.Embed, in.Result); err != nil {
			return retry("process_url "+task.AssetID, err)
		}
		return p.embedNext(ctx, task, &st, in.lastAttempt())
	default:
		return done("skipped_bad_state")
	}
}

func scrapeRequest(in ProcessURLTask) firecrawl.ScrapeRequest {
	req := firecrawl.ScrapeRequest{URL: in.SourceURL}
	if in.Refresh {
		// On refresh, bypass Firecrawl's cache.
		req.MaxAge = new(0)
	}
	return req
}

func (p *ProcessURLProcessor) start(ctx context.Context, in ProcessURLTask, lastAttempt bool) (StepOutput, error) {
	if p.Deps.Scraper == nil {
		slog.WarnContext(ctx, "firecrawl not configured", logging.AttrComponent, "jobs.process_url", "asset_id", in.AssetID)
		return done("skipped_unconfigured")
	}
	giveUp := func() error { return p.finish(ctx, in, models.AssetStatusFailed, "", 0, 0, 0, urlEmbedderUnavailable) }
	if ok, err := requireEmbedder(ctx, p.Deps.Embedder, "process_url", in.AssetID, lastAttempt, giveUp); !ok {
		return settledOr(err)
	}
	if err := p.statusWriter().set(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return retry("process_url", err)
	}
	p.publish(ctx, in, models.AssetStatusProcessing, "", 0, 0, 0, "")

	req := scrapeRequest(in)
	call, err := firecrawl.Plan(func(c *firecrawl.Client) error { _, err := c.Scrape(ctx, req); return err })
	if err != nil {
		return StepOutput{}, err
	}
	raw, err := encodeState(urlState{Phase: urlPhaseScrape})
	if err != nil {
		return StepOutput{}, err
	}
	scraper := p.Deps.Scraper
	return StepOutput{State: raw, HTTP: &HTTPCall{
		Upstream: UpstreamFirecrawl, Method: call.Method, Path: call.Path, Body: call.Body,
		local: func(ctx context.Context) (any, error) { return scraper.Scrape(ctx, req) },
	}}, nil
}

// scraped settles the scrape: a transient failure retries, an unscrapeable
// page fails the asset. Every successful scrape is a billable credit, so it
// is metered here, which stays accurate across retries.
func (p *ProcessURLProcessor) scraped(ctx context.Context, in ProcessURLTask, r *StepResult) (StepOutput, error) {
	req := scrapeRequest(in)
	res, err := localOr(r, func(r *StepResult) (*firecrawl.ScrapeResult, error) {
		var res *firecrawl.ScrapeResult
		err := firecrawl.Decode(firecrawl.Result{Status: r.Status, Body: r.Body, Err: r.Err}, func(c *firecrawl.Client) error {
			var err error
			res, err = c.Scrape(ctx, req)
			return err
		})
		return res, err
	})
	if err != nil {
		if firecrawl.IsTransient(err) {
			return retry("process_url "+in.AssetID+": scrape", err)
		}
		slog.WarnContext(ctx, "unscrapeable url", logging.AttrComponent, "jobs.process_url", "asset_id", in.AssetID, logging.AttrError, err)
		return settledOr(p.finish(ctx, in, models.AssetStatusFailed, "", 0, 0, 0, err.Error()))
	}
	p.Deps.Recorder.Record(ctx, firecrawl.VendorFirecrawl, "url_scrape", vendors.MeterEvent{
		Model:     "scrape",
		Operation: firecrawl.OpScrape,
		Usage:     vendors.Usage{vendors.KindURLScrape: 1},
	})
	if t, ok := pubdate.Web(time.Now(), res.PublishedTime...); ok {
		recordPublishedAt(ctx, p.Deps.Published, "process_url", in.AssetID, t)
	}
	md, err := gzipText(res.Markdown)
	if err != nil {
		return StepOutput{}, err
	}
	st := &urlState{Phase: urlPhaseImages, Title: cmp.Or(strings.TrimSpace(res.Title), in.SourceURL), Markdown: md}
	if p.Deps.Storage != nil {
		st.Refs = imageRefs(ctx, in.AssetID, res.Markdown)
	}
	return p.nextImage(ctx, in, st)
}

// nextImage fetches the next page image, or, once all are mirrored, rewrites
// the page's links, persists it and starts embedding.
func (p *ProcessURLProcessor) nextImage(ctx context.Context, in ProcessURLTask, st *urlState) (StepOutput, error) {
	if st.Next < len(st.Refs) {
		ref := st.Refs[st.Next]
		raw, err := encodeState(st)
		if err != nil {
			return StepOutput{}, err
		}
		fetcher := p.fetcher()
		return StepOutput{State: raw, HTTP: &HTTPCall{
			Method: http.MethodGet, URL: ref.URL,
			local: func(ctx context.Context) (any, error) {
				data, ct, err := fetcher.Fetch(ctx, ref.URL)
				return fetchedImage{data: data, contentType: ct}, err
			},
		}}, nil
	}
	markdown, err := gunzipText(st.Markdown)
	if err != nil {
		return StepOutput{}, err
	}
	markdown = replaceLinks(markdown, st.Replacements)
	if err := p.persistContent(ctx, in.AssetID, st.Title, markdown, st.Images); err != nil {
		return retry("process_url", err)
	}
	runID, err := models.NewID()
	if err != nil {
		return StepOutput{}, err
	}
	// The title is prepended for context, like embed_asset.
	stages := stageSources(textChunkSources(flows.ChunkText(st.Title + "\n\n" + markdown)))
	if err := p.stages().Begin(ctx, runID, in.AssetID, stages); err != nil {
		return retry("process_url "+in.AssetID+": stage chunks", err)
	}
	next := &urlState{Phase: urlPhaseEmbed, Title: st.Title, Images: st.Images, FailedImages: st.FailedImages,
		Embed: embedProgress{RunID: runID}, Empty: len(stages) == 0}
	return p.embedNext(ctx, in, next, false)
}

// fetchedImage is a page image as downloaded.
type fetchedImage struct {
	data        []byte
	contentType string
}

// fetched mirrors the image just downloaded. It is best-effort: a failed
// image keeps its original external link and counts towards the failures
// that flip the asset to "partial".
func (p *ProcessURLProcessor) fetched(ctx context.Context, in ProcessURLTask, st *urlState, r *StepResult) {
	ref := st.Refs[st.Next]
	st.Next++
	img, err := localOr(r, func(r *StepResult) (fetchedImage, error) {
		switch {
		case r.Err != "":
			return fetchedImage{}, errors.New(r.Err)
		case r.Status != http.StatusOK:
			return fetchedImage{}, fmt.Errorf("image fetch %s: status %d", ref.URL, r.Status)
		case len(r.Body) > maxImageBytes:
			return fetchedImage{}, fmt.Errorf("image %s exceeds %d bytes", ref.URL, maxImageBytes)
		}
		return fetchedImage{data: r.Body, contentType: r.ContentType}, nil
	})
	if err != nil {
		st.FailedImages++
		return
	}
	stored, publicURL, ok := p.mirrorImage(ctx, in.AssetID, st.Next-1, ref.URL, ref.Alt, img)
	switch {
	case !ok:
		st.FailedImages++
	case stored != nil:
		if st.Replacements == nil {
			st.Replacements = map[string]string{}
		}
		stored.Idx = len(st.Images)
		st.Images = append(st.Images, *stored)
		st.Replacements[ref.URL] = publicURL
	}
}

// embedNext embeds the next batch, or, once every chunk is settled, commits
// the chunks and settles the asset.
func (p *ProcessURLProcessor) embedNext(ctx context.Context, in ProcessURLTask, st *urlState, lastAttempt bool) (StepOutput, error) {
	call, err := p.embedLoop().next(ctx, &st.Embed)
	if err != nil {
		return retry("process_url "+in.AssetID, err)
	}
	if call != nil {
		raw, err := encodeState(st)
		if err != nil {
			return StepOutput{}, err
		}
		return StepOutput{State: raw, HTTP: call}, nil
	}
	// Chunks left over from the page's earlier content are cleared when this
	// run staged nothing. Only then: a run whose stages a later, overlapping
	// run dropped also counts no attempts, and must not clear the chunks.
	committed, err := p.stages().Commit(ctx, st.Embed.RunID, in.AssetID, st.Empty)
	if err != nil {
		return retry("process_url "+in.AssetID+": store chunks", err)
	}
	stats := stageStats(committed)
	final, err := stats.settle(lastAttempt)
	if err != nil {
		return retry("process_url "+in.AssetID, err)
	}
	switch {
	case final == models.AssetStatusFailed:
		return settledOr(p.finish(ctx, in, final, st.Title, len(st.Images), 0, st.FailedImages, urlAllChunksFailed))
	case final == models.AssetStatusReady && st.FailedImages > 0:
		final = models.AssetStatusPartial
	}
	if err := p.finish(ctx, in, final, st.Title, len(st.Images), stats.Embedded, st.FailedImages, ""); err != nil {
		return retry("process_url", err)
	}
	// After the durable status write, so a retry from a late failure can't
	// double-count.
	if stats.Tokens > 0 {
		p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "url_embed", llm.EmbedUsage{Tokens: stats.Tokens})
	}
	return done(final)
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
	return assetStatusWriter{op: "process_url", assets: p.Deps.Assets, notifier: p.Deps.Notifier, label: "link", kind: models.AssetTypeURL, analysis: p.Deps.Analysis}
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

// mirrorImage re-hosts one fetched image under storage key slot pos.
// ok is false on a failure; a nil img with ok true is a deliberate skip.
func (p *ProcessURLProcessor) mirrorImage(ctx context.Context, assetID string, pos int, url, alt string, fetched fetchedImage) (img *models.AssetImage, publicURL string, ok bool) {
	data, ct := fetched.data, fetched.contentType
	if !strings.HasPrefix(ct, "image/") {
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
	publicURL, err := p.Deps.Storage.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), ct)
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

// imageRefs lists the page's http(s) images in page order, each once, at
// most maxImagesPerAsset.
func imageRefs(ctx context.Context, assetID, markdown string) []imageRef {
	seen := make(map[string]bool)
	var refs []imageRef
	add := func(u, alt string) {
		if !isHTTPURL(u) || seen[u] {
			return
		}
		seen[u] = true
		refs = append(refs, imageRef{URL: u, Alt: alt})
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
	return refs
}

// replaceLinks rewrites mirrored images' links to their stored URLs,
// longest-key-first (deterministic) so a source URL that is a prefix of
// another (e.g. ".../a.png" vs ".../a.png?w=100") can't clobber the longer
// one before it is itself replaced.
func replaceLinks(markdown string, replacements map[string]string) string {
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
	return out
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
