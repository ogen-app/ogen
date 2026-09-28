package server

import (
	"context"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/infra/embedding"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/jobs/queues"
	audioclient "github.com/ogen-app/ogen/src/transport/grpc/client/audio"
	"github.com/ogen-app/ogen/src/transport/grpc/client/documents"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
	"github.com/ogen-app/ogen/src/transport/grpc/client/video"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// grpcClients are the optional microservice clients, reached over the private
// network. Each is nil when its *_SERVICE_ADDR is unset.
type grpcClients struct {
	pdf       *pdf.Client
	video     *video.Client
	documents *documents.Client
	audio     *audioclient.Client
	image     *imageclient.Client
}

// optionalClient is a gRPC client constructor's result: nil when the service
// is not configured.
type optionalClient interface {
	comparable
	Close() error
}

// dialOptional builds one optional gRPC client and, when the service is
// configured, schedules its Close in the given shutdown stage.
func dialOptional[C optionalClient](plan *shutdownPlan, stage shutdownStage, dial func() (C, error)) (C, error) {
	var zero C
	c, err := dial()
	if err != nil {
		return zero, err
	}
	if c != zero {
		plan.add(stage, c.Close)
	}
	return c, nil
}

// newGRPCClients dials the optional microservices. Every client a River job
// calls (pdf, documents, audio, image) closes in stageJobClients, after the
// jobs have drained; video is request-time only.
func newGRPCClients(d *deps) (grpcClients, error) {
	cfg := d.cfg
	var c grpcClients
	var err error
	if c.pdf, err = dialOptional(d.shutdown, stageJobClients, func() (*pdf.Client, error) {
		return pdf.New(pdf.Config{Addr: cfg.PDFServiceAddr, Timeout: cfg.PDFServiceTimeout, MaxRecvBytes: cfg.PDFServiceMaxRecvBytes})
	}); err != nil {
		return c, err
	}
	if c.video, err = dialOptional(d.shutdown, stageIntegrations, func() (*video.Client, error) {
		return video.New(video.Config{Addr: cfg.VideoServiceAddr, Timeout: cfg.VideoServiceTimeout, MaxRecvBytes: cfg.VideoServiceMaxRecvBytes})
	}); err != nil {
		return c, err
	}
	if c.documents, err = dialOptional(d.shutdown, stageJobClients, func() (*documents.Client, error) {
		return documents.New(documents.Config{Addr: cfg.DocumentsServiceAddr, Timeout: cfg.DocumentsServiceTimeout, MaxRecvBytes: cfg.DocumentsServiceMaxRecvBytes})
	}); err != nil {
		return c, err
	}
	if c.audio, err = dialOptional(d.shutdown, stageJobClients, func() (*audioclient.Client, error) {
		return audioclient.New(audioclient.Config{Addr: cfg.AudioServiceAddr, Timeout: cfg.AudioServiceTimeout, MaxRecvBytes: cfg.AudioServiceMaxRecvBytes})
	}); err != nil {
		return c, err
	}
	if c.image, err = dialOptional(d.shutdown, stageJobClients, func() (*imageclient.Client, error) {
		return imageclient.New(imageclient.Config{Addr: cfg.ImageServiceAddr, Timeout: cfg.ImageServiceTimeout, MaxRecvBytes: cfg.ImageServiceMaxRecvBytes})
	}); err != nil {
		return c, err
	}
	return c, nil
}

// The accessors below return a true nil interface for an unwired service:
// a typed-nil *Client would make the interface non-nil and defeat the
// handlers' `== nil` guards.

func (c grpcClients) pdfRenderer() handlers.PDFRenderer {
	if c.pdf == nil {
		return nil
	}
	return c.pdf
}

func (c grpcClients) videoProber() handlers.VideoProber {
	if c.video == nil {
		return nil
	}
	return c.video
}

func (c grpcClients) imagePreparer() handlers.ImagePreparer {
	if c.image == nil {
		return nil
	}
	return c.image
}

// ingestion is the content-bank ingestion wiring. A file kind is enabled when
// its parser client and object storage are both configured; a disabled kind's
// worker deps keep a nil Client so the worker no-ops. Embedder availability is
// checked per run, so a Gemini key added later enables embedding without a
// restart.
type ingestion struct {
	embedCallbacks embedding.Callbacks
	embedder       ai.Embedder
	// firecrawl resolves its key per request: with no key, the process_url
	// worker and the /url endpoint stay dormant (409).
	firecrawl *firecrawl.Client

	pdfOn, documentOn, audioOn, imageOn bool

	pdf      queues.PDFDeps
	document queues.DocumentDeps
	audio    queues.AudioDeps
	image    queues.ImageDeps
	url      queues.URLDeps
}

// newIngestion builds the per-kind River worker deps.
func newIngestion(d *deps, callbacks embedding.Callbacks, embedder ai.Embedder) ingestion {
	cfg, r, store, rec := d.cfg, d.r, d.store, d.usage.recorder
	in := ingestion{
		embedCallbacks: callbacks,
		embedder:       embedder,
		firecrawl: firecrawl.New(
			func(ctx context.Context) (string, error) { return d.secretStore.Get(ctx, secrets.NameFirecrawlAPIKey) },
			cfg.FirecrawlBaseURL, cfg.FirecrawlHTTPTimeout,
		),
		pdfOn:      d.clients.pdf != nil && store != nil,
		documentOn: d.clients.documents != nil && store != nil,
		audioOn:    d.clients.audio != nil && store != nil,
		imageOn:    d.clients.image != nil && store != nil,
		pdf: queues.PDFDeps{
			Embedder:   embedder,
			Storage:    store,
			Assets:     r.pieceRepo,
			Chunks:     r.chunksRepo,
			Files:      r.assetFileRepo,
			Recorder:   rec,
			EmbedModel: cfg.EmbedModel,
			Notifier:   d.notifier,
		},
		document: queues.DocumentDeps{
			Embedder:   embedder,
			Storage:    store,
			Assets:     r.pieceRepo,
			Chunks:     r.chunksRepo,
			Files:      r.assetFileRepo,
			Recorder:   rec,
			EmbedModel: cfg.EmbedModel,
			Notifier:   d.notifier,
		},
		audio: queues.AudioDeps{
			Embedder:         embedder,
			Storage:          store,
			Assets:           r.pieceRepo,
			Content:          r.pieceRepo,
			Chunks:           r.chunksRepo,
			Extractions:      r.audioExtractionRepo,
			Segments:         r.audioSegmentRepo,
			Utterances:       r.utteranceRepo,
			Recorder:         rec,
			Checker:          d.usage.checker,
			EmbedModel:       cfg.EmbedModel,
			TranscribeModel:  cfg.TranscribeModel,
			SegmentMaxMs:     cfg.AudioSegmentMaxMs,
			SegmentOverlapMs: cfg.AudioSegmentOverlapMs,
			MaxDurationMs:    cfg.AudioMaxDurationMs,
			JobTimeout:       cfg.AudioJobTimeout,
			Notifier:         d.notifier,
		},
		image: queues.ImageDeps{
			Embedder:            embedder,
			Storage:             store,
			Assets:              r.pieceRepo,
			Chunks:              r.chunksRepo,
			Files:               r.assetFileRepo,
			Extractions:         r.imageExtractionRepo,
			Blocks:              r.imageBlockRepo,
			Recorder:            rec,
			Checker:             d.usage.checker,
			EmbedModel:          cfg.EmbedModel,
			ClassifyModel:       cfg.VisionClassifyModel,
			ExtractModel:        cfg.VisionExtractModel,
			EscalateModel:       cfg.VisionEscalateModel,
			ConfidenceThreshold: cfg.VisionConfidenceThreshold,
			AltTextMaxChars:     cfg.AltTextGenMaxChars,
			JobTimeout:          cfg.ImageJobTimeout,
			Notifier:            d.notifier,
		},
	}
	// Without storage, scraped images stay external links.
	in.url = queues.URLDeps{
		Scraper:    in.firecrawl,
		Embedder:   embedder,
		Storage:    store,
		Assets:     r.pieceRepo,
		Chunks:     r.chunksRepo,
		Images:     r.assetImageRepo,
		Hub:        d.hub,
		Recorder:   rec,
		EmbedModel: cfg.EmbedModel,
		Notifier:   d.notifier,
	}
	in.wireClients(d.clients)
	return in
}

// wireClients sets each enabled kind's parser client, leaving a disabled
// kind's Client a true nil.
func (in *ingestion) wireClients(c grpcClients) {
	if in.pdfOn {
		in.pdf.Client = c.pdf
	}
	if in.documentOn {
		in.document.Client = c.documents
	}
	if in.audioOn {
		in.audio.Client = c.audio
	}
	if in.imageOn {
		in.image.Client = c.image
	}
}
