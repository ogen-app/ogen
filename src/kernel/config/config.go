package config

import (
	"time"

	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	Addr  string `envconfig:"ADDR"                default:":9001"`
	DSN   string `envconfig:"DATABASE_DSN"        default:"postgres://ogen:ogen@localhost:5432/ogen?sslmode=disable"`
	Debug bool   `envconfig:"DEBUG"               default:"false"`

	// Structured logging (CON-107). LogLevel is the minimum emitted level
	// (debug|info|warn|error; unknown/empty ⇒ info). LogFormat selects the
	// slog handler: json|text. LogFormat is intentionally left empty by
	// default so it can resolve per-environment — empty means "text when
	// DEBUG=true (local ergonomics), JSON otherwise"; an explicit value always
	// wins. See src/logging.
	LogLevel  string `envconfig:"LOG_LEVEL"  default:"info"`
	LogFormat string `envconfig:"LOG_FORMAT" default:""`

	// AnthropicDebugHTTP (CON-112 perf diagnostics) logs every Anthropic
	// round-trip split into httptrace phases (conn vs. server time), so a slow
	// call can be attributed to connection acquisition vs. the API itself. This
	// is how the ~50-60s per-request latency was traced to server-side
	// strict-tool-schema (grammar) recompilation — see AnthropicStableToolOrder
	// for the root cause and fix. Diagnostic only; leave off in production.
	AnthropicDebugHTTP bool `envconfig:"ANTHROPIC_DEBUG_HTTP" default:"false"`

	// AnthropicStableToolOrder (CON-112 fix) sorts the outgoing tools array by
	// name on every Anthropic /v1/messages request. The Genkit Anthropic plugin
	// builds the tool list by ranging a Go map (ai/generate.go) — a *random*
	// order per request — and forces strict:true on every tool. Anthropic
	// compiles strict tool schemas into a constrained-decoding grammar cached
	// per exact tool-set (the cache key is order-sensitive), so a random order
	// misses that cache on every call and pays the full ~50s compile each time.
	// A deterministic order keeps the cache warm after the first request. On by
	// default; set false only to A/B the effect.
	AnthropicStableToolOrder bool `envconfig:"ANTHROPIC_STABLE_TOOL_ORDER" default:"true"`

	// EnablePprof (CON-112 perf diagnostics) serves net/http/pprof on
	// localhost:6060 (container-internal; reach via `docker compose exec`). A
	// goroutine dump captured during a slow request shows exactly what every
	// goroutine is doing. Diagnostic only; leave off in production.
	EnablePprof bool `envconfig:"ENABLE_PPROF" default:"false"`

	// Connection-pool sizing. Postgres lifts SQLite's single-writer
	// ceiling, so the API runs a real pool shared by bun and the River
	// job queue. Size MaxOpen for combined HTTP + worker load.
	DBMaxOpenConns    int    `envconfig:"DB_MAX_OPEN_CONNS" default:"25"`
	DBMaxIdleConns    int    `envconfig:"DB_MAX_IDLE_CONNS" default:"5"`
	SessionCookieName string `envconfig:"SESSION_COOKIE_NAME" default:"c3_session"`

	// Embeddings (CON-101). Generated via the hosted Gemini Embedding 2 API
	// (google.golang.org/genai through the Genkit googlegenai plugin), replacing
	// the former self-hosted embedding sidecar. GeminiAPIKey is now a
	// first-boot seed source only (CON-104): it is migrated into the encrypted
	// `secret` table on startup and thereafter read from — and rotated via — the
	// gRPC secrets service (secrets.NameGeminiAPIKey), mirroring the Anthropic /
	// Zernio keys. With no key the embedder is unavailable (asset saves succeed,
	// no vectors are written, semantic search returns nothing) until one is set.
	// EmbedDimensions must match the assets_chunks.embedding halfvec(N) column —
	// 3072 is Gemini's native size and is L2-normalized, so cosine search works
	// without renormalization.
	//GeminiAPIKey    string `envconfig:"GEMINI_API_KEY"    default:""`
	EmbedModel      string `envconfig:"EMBED_MODEL"       default:"gemini-embedding-2"`
	EmbedDimensions int    `envconfig:"EMBED_DIMENSIONS"  default:"3072"`

	// CORS allowlist for the decoupled UI (CON-98). Comma-separated explicit
	// origins, e.g. "https://app.getogen.com". Empty disables the CORS
	// middleware entirely (same-origin dev, or a UI that reverse-proxies
	// /api). Must never be "*" while credentials are sent — the cookie-bearing
	// UI requires AllowCredentials, which browsers reject alongside a wildcard.
	CORSAllowedOrigins string `envconfig:"CORS_ALLOWED_ORIGINS" default:""`

	// Anthropic config.
	// todo: remove from config entirely
	AnthropicAPIKey  string `envconfig:"ANTHROPIC_API_KEY"     default:""`
	ModelID          string `envconfig:"MODEL_ID"              default:"claude-sonnet-4-5-20250929"` // claude-haiku-4-5-20251001 for testing
	MaxContextAssets int    `envconfig:"MAX_ASSET_CONTEXT"     default:"15"`
	MaxContextChars  int    `envconfig:"MAX_CONTEXT_CHARS"     default:"10000"`

	// PlanningModelID (CON-112) backs the cheap/fast "planning" role used by
	// the Campaign Assistant's orchestration + intent-routing loop. Prose
	// generation happens inside the content_plan / enrich_brief sub-flows it
	// invokes as tools, which stay on ModelID (Sonnet-tier) — so the assistant
	// routes cheaply on Haiku while the heavy writing stays capable.
	PlanningModelID string `envconfig:"PLANNING_MODEL_ID" default:"claude-haiku-4-5-20251001"`

	// 64K matches Claude 4.x Haiku/Sonnet's max output. Anthropic charges
	// only for tokens actually emitted, so a generous cap costs nothing on
	// short responses but prevents truncation on long rewrites (assistant
	// flow with explanation + full post content + tool inputs combined).
	MaxOutputTokens int64 `envconfig:"MAX_OUTPUT_TOKENS"     default:"64000"`

	// PostAssistantPlanner (CON-128) enables the hybrid model split for the
	// Post Assistant: the orchestration/routing loop runs on the cheap
	// PlanningModelID (Haiku) while the actual copywriting is delegated to a
	// Sonnet (ModelID) editPost write-tool. Default on. Set to false to force
	// the whole assistant back onto the proven single-Sonnet path (loop on
	// ModelID, no editPost tool, inline content) — the instant rollback lever
	// if Haiku routing regresses. Model ids stay tunable via MODEL_ID /
	// PLANNING_MODEL_ID regardless.
	PostAssistantPlanner bool `envconfig:"POST_ASSISTANT_PLANNER" default:"true"`

	// PostAssistantPlannerMaxOutputTokens caps the Haiku planner turn's output
	// (CON-128). The planner only emits a short envelope (explanation + action
	// + saveVersion + versionNote) plus tool inputs, so a small cap is plenty;
	// the full post is produced by the writer sub-call under MaxOutputTokens.
	// 0 falls back to a sensible default (8192).
	PostAssistantPlannerMaxOutputTokens int64 `envconfig:"POST_ASSISTANT_PLANNER_MAX_OUTPUT_TOKENS" default:"8192"`

	// Content-plan batching. The flow generates posts in K-sized batches in
	// parallel; the defaults are sized so a 64K-output Sonnet call comfortably
	// returns 30 posts with headroom, and so an account with default tier
	// limits doesn't stall on parallel ITPM bursts. Tune up for plans with
	// 200+ posts where wall time matters.
	MaxPostsPerBatch   int `envconfig:"MAX_POSTS_PER_BATCH"   default:"30"`
	MaxParallelBatches int `envconfig:"MAX_PARALLEL_BATCHES"  default:"5"`

	// GeneratePostsMax caps the Campaign Assistant's targeted "add a few posts"
	// tool (CON-114) so it stays an incremental add rather than a full re-plan.
	GeneratePostsMax int `envconfig:"GENERATE_POSTS_MAX" default:"10"`

	// DraftPostMax caps the Campaign Assistant's draftPost tool (CON-207) — how
	// many finished, content-first drafts one "create a post from this research"
	// call may produce (total across platforms). Smaller than GeneratePostsMax:
	// each draft is full-length copy, not a terse thesis.
	DraftPostMax int `envconfig:"DRAFT_POST_MAX" default:"5"`

	// ConsistencyPostsMax caps how many posts the Campaign Assistant's posts-vs-
	// brief consistency check (CON-116) analyzes in a single model call.
	ConsistencyPostsMax int `envconfig:"CONSISTENCY_POSTS_MAX" default:"20"`

	// Post quality assessment (CON-85). Scoring runs on Sonnet 4.5 by
	// default — Haiku underdelivered (terse, omitting per-dimension prose) —
	// specified separately from ModelID so the scoring model can be tuned
	// independently. QualityWeightProfiles is an optional JSON override for
	// the per-PlatformPostType weight profiles; empty uses the built-in
	// defaults (post_quality.DefaultWeights). Each profile's four weights
	// must sum to 1.0. Example:
	//   {"profiles":{"reel":{"correctness":0.2,"clarity":0.15,"engagement":0.4,"delivery":0.25}}}
	QualityModelID        string `envconfig:"QUALITY_MODEL_ID"        default:"claude-sonnet-4-5-20250929"`
	QualityWeightProfiles string `envconfig:"QUALITY_WEIGHT_PROFILES" default:""`

	// Object storage (S3-compatible: Cloudflare R2, DigitalOcean Spaces, AWS S3).
	// Leave StorageEndpoint empty to disable image uploads.
	StorageEndpoint  string `envconfig:"STORAGE_ENDPOINT"   default:""`
	StorageRegion    string `envconfig:"STORAGE_REGION"     default:"auto"`
	StorageAccessKey string `envconfig:"STORAGE_ACCESS_KEY" default:""`
	StorageSecretKey string `envconfig:"STORAGE_SECRET_KEY" default:""`
	StorageBucket    string `envconfig:"STORAGE_BUCKET"     default:""`
	StoragePublicURL string `envconfig:"STORAGE_PUBLIC_URL" default:""` // CDN/public base URL for returned object URLs

	// PDF parsing microservice (CON-103). The API streams PDF bytes to
	// pdf-service over gRPC — exclusively over the Railway private network — and
	// gets back page-attributed chunks + a thumbnail. Empty PDFServiceAddr
	// disables PDF ingestion (uploads accepted, parsing skipped), mirroring the
	// empty-key pattern above. In prod this is the private hostname
	// (pdf-service.railway.internal:50051); compose/tests use pdf-service:50051.
	// MaxRecvBytes raises the gRPC client receive cap (the response carries chunk
	// text + thumbnail, which can exceed gRPC's 4MB default) — 64 MiB here.
	PDFServiceAddr         string        `envconfig:"PDF_SERVICE_ADDR"           default:""`
	PDFServiceTimeout      time.Duration `envconfig:"PDF_SERVICE_TIMEOUT"        default:"2m"`
	PDFServiceMaxRecvBytes int           `envconfig:"PDF_SERVICE_MAX_RECV_BYTES" default:"67108864"`

	// Video probing microservice (CON-148), mirroring pdf-service. The API
	// hands video-service a short-lived presigned GET URL over gRPC — over the
	// Railway private network — and gets back duration/codec/resolution + a
	// poster frame. Empty VideoServiceAddr disables probing (uploads accepted
	// but unprobed: no duration/poster, weaker validation), mirroring the
	// empty-addr pattern above. Prod: video-service.railway.internal:50051;
	// compose/tests: video-service:50051. MaxRecvBytes raises the gRPC client
	// receive cap so a poster PNG can exceed gRPC's 4MB default — 64 MiB here.
	VideoServiceAddr         string        `envconfig:"VIDEO_SERVICE_ADDR"           default:""`
	VideoServiceTimeout      time.Duration `envconfig:"VIDEO_SERVICE_TIMEOUT"        default:"2m"`
	VideoServiceMaxRecvBytes int           `envconfig:"VIDEO_SERVICE_MAX_RECV_BYTES" default:"67108864"`

	// Document parsing microservice (CON-280), mirroring pdf-service. The API
	// streams office/text document bytes (.docx/.pptx/.xlsx/.odt/... ) to
	// document-service over gRPC — over the Railway private network — and gets
	// back embedding-ready, source-anchored chunks. Empty DocumentsServiceAddr
	// disables document ingestion (those uploads fail fast with a clear message),
	// mirroring the empty-addr pattern above. Prod:
	// document-service.railway.internal:50051; compose/tests: document-service:50051.
	// A larger receive cap + longer timeout than PDF: a spreadsheet can explode
	// into many labelled row chunks (128 MiB / 5m here).
	DocumentsServiceAddr         string        `envconfig:"DOCUMENTS_SERVICE_ADDR"           default:""`
	DocumentsServiceTimeout      time.Duration `envconfig:"DOCUMENTS_SERVICE_TIMEOUT"        default:"5m"`
	DocumentsServiceMaxRecvBytes int           `envconfig:"DOCUMENTS_SERVICE_MAX_RECV_BYTES" default:"134217728"`

	// Zernio integration. Empty ZernioAPIKey disables the
	// integration entirely; everything else stays defaulted.
	ZernioAPIKey           string        `envconfig:"ZERNIO_API_KEY"            default:""`
	ZernioBaseURL          string        `envconfig:"ZERNIO_BASE_URL"           default:"https://zernio.com/api/v1"`
	ZernioHTTPTimeout      time.Duration `envconfig:"ZERNIO_HTTP_TIMEOUT"       default:"15s"`
	ZernioSyncInterval     time.Duration `envconfig:"ZERNIO_SYNC_INTERVAL"      default:"30s"`
	ZernioSyncIntervalFast time.Duration `envconfig:"ZERNIO_SYNC_INTERVAL_FAST" default:"5s"`

	// ZernioEnv namespaces each tenant's Zernio profile name,
	// "Ogen-<ZernioEnv>-<tenant_id>" (CON-102), so profiles created by dev,
	// staging, and prod Ogen instances against the same shared Zernio account
	// stay distinguishable on the Zernio dashboard. Defaults to "dev".
	ZernioEnv string `envconfig:"ZERNIO_ENV" default:"dev"`

	// Analytics refresh (CON-93 §6 FR3). The refresh_zernio_analytics
	// queue batch-fetches engagement analytics on this cadence and only
	// considers posts published within the lookback window (Zernio caps
	// the analytics range at 366 days). CON-236 made this the BASE (finest)
	// tick and added a per-post age-based decay gate on top, so the default
	// moved from 30m to 1h (matching the fresh-bucket cadence).
	ZernioAnalyticsRefreshInterval time.Duration `envconfig:"ZERNIO_ANALYTICS_REFRESH_INTERVAL" default:"1h"`
	ZernioAnalyticsWindowDays      int           `envconfig:"ZERNIO_ANALYTICS_WINDOW_DAYS"      default:"90"`

	// Analytics refresh decay schedule (CON-236). A post is re-checked more often
	// while it's young and its numbers still move, then progressively less. The
	// windows delimit the age buckets (fresh < FreshWindow, warm < WarmWindow,
	// else cold); each *Every is that bucket's re-check interval. A zero *Every
	// disables gating for its bucket (always due). Setting all three to the base
	// interval reverts to "check every tick".
	ZernioAnalyticsFreshWindow time.Duration `envconfig:"ZERNIO_ANALYTICS_FRESH_WINDOW" default:"48h"`
	ZernioAnalyticsWarmWindow  time.Duration `envconfig:"ZERNIO_ANALYTICS_WARM_WINDOW"  default:"336h"`
	ZernioAnalyticsFreshEvery  time.Duration `envconfig:"ZERNIO_ANALYTICS_FRESH_EVERY"  default:"1h"`
	ZernioAnalyticsWarmEvery   time.Duration `envconfig:"ZERNIO_ANALYTICS_WARM_EVERY"   default:"24h"`
	ZernioAnalyticsColdEvery   time.Duration `envconfig:"ZERNIO_ANALYTICS_COLD_EVERY"   default:"168h"`

	// Follower-stats refresh (CON-153). The refresh_zernio_followers queue
	// snapshots each connected account's follower count on this cadence. Zernio
	// refreshes follower counts once per day, so a 24h default matches the
	// upstream granularity. ZernioFollowerRetentionDays carries the retention
	// default for parity with the analytics-DB policy; the migration installs a
	// fixed policy (operators adjust it out of band, like UsageRetentionDays).
	ZernioFollowerRefreshInterval time.Duration `envconfig:"ZERNIO_FOLLOWER_REFRESH_INTERVAL" default:"24h"`
	ZernioFollowerRetentionDays   int           `envconfig:"ZERNIO_FOLLOWER_RETENTION_DAYS"   default:"365"`

	// Connection-expiry notifications (CON-219). The detect_expiring_connections
	// queue reads each connected account's Zernio health on this cadence and
	// emails workspace owners when a token is within ConnectionExpiryLeadDays of
	// expiry (or already needs reconnecting). Profile-driven, so it no-ops when
	// Zernio is unconfigured. A 6h cadence gives ~28 checks across a 7-day lead
	// window; durable per-(account,stage,expiry,owner) dedupe (via email_logs)
	// keeps that to one notification each.
	ZernioHealthCheckInterval time.Duration `envconfig:"ZERNIO_HEALTH_CHECK_INTERVAL" default:"6h"`
	ConnectionExpiryLeadDays  int           `envconfig:"CONNECTION_EXPIRY_LEAD_DAYS"  default:"7"`

	// Optional post-OAuth redirect target. When set, every connect
	// link Zernio issues will send the user here after authorization
	// succeeds, with ?connected=<platform>&profileId=<id>&accountId=<id>&username=<name>
	// appended by Zernio. Leaving this empty falls back to Zernio's
	// default success page.
	ZernioRedirectURL string `envconfig:"ZERNIO_REDIRECT_URL" default:""`

	// Firecrawl web-scrape (CON-222). URL assets are ingested by scraping the
	// page to Markdown via the Firecrawl.dev API (POST /v2/scrape). Like the
	// Anthropic / Zernio / Resend keys, FirecrawlAPIKey is a first-boot seed
	// source only: migrated into the encrypted `secret` table on startup and
	// thereafter read from — and rotated via — the gRPC secrets service
	// (secrets.NameFirecrawlAPIKey), so the scrape client resolves the current
	// key per request with no restart. Empty key disables URL ingestion (the
	// endpoint returns 409). The 90s timeout covers Firecrawl's server-side
	// render, which its API caps at 60s.
	FirecrawlAPIKey      string        `envconfig:"FIRECRAWL_API_KEY"      default:""`
	FirecrawlBaseURL     string        `envconfig:"FIRECRAWL_BASE_URL"     default:"https://api.firecrawl.dev"`
	FirecrawlHTTPTimeout time.Duration `envconfig:"FIRECRAWL_HTTP_TIMEOUT" default:"90s"`

	// Email (CON-154). Transactional + marketing mail via Resend. ResendAPIKey,
	// ResendWebhookSecret, and EmailLinkSecret are first-boot seed sources only:
	// migrated into the encrypted `secret` table on startup and thereafter read
	// from — and rotated via — the gRPC secrets service, mirroring the Anthropic /
	// Zernio keys. Empty ResendAPIKey disables sending entirely (the send job logs
	// skipped_disabled and succeeds); the app still boots. EmailLinkSecret signs
	// unsubscribe links; when unset it is auto-generated and stored on first boot
	// (secrets.EnsureGenerated), so one-click unsubscribe works out of the box.
	ResendAPIKey        string `envconfig:"RESEND_API_KEY"           default:""`
	ResendWebhookSecret string `envconfig:"RESEND_WEBHOOK_SECRET"    default:""`
	EmailLinkSecret     string `envconfig:"EMAIL_LINK_SECRET"        default:""`
	// EmailFrom is the RFC 5322 From header; operators must point it at a
	// Resend-verified sending domain (SPF/DKIM). EmailBaseURL is Resend's API
	// root. AppBaseURL builds absolute unsubscribe / CTA links in emails.
	EmailFrom        string        `envconfig:"EMAIL_FROM"                default:"Ogen <hello@getogen.com>"`
	EmailReplyTo     string        `envconfig:"EMAIL_REPLY_TO"            default:""`
	EmailBaseURL     string        `envconfig:"EMAIL_BASE_URL"            default:"https://api.resend.com"`
	EmailHTTPTimeout time.Duration `envconfig:"EMAIL_HTTP_TIMEOUT"        default:"15s"`
	AppBaseURL       string        `envconfig:"APP_BASE_URL"              default:"https://app.getogen.com"`
	// EmailLinkBaseURL (CON-155) is the base for public unsubscribe links; it must
	// resolve to the API host. Empty falls back to AppBaseURL — fine for
	// same-origin / reverse-proxied deploys; set explicitly for a split-origin
	// deploy where the SPA and API live on different hosts.
	EmailLinkBaseURL string `envconfig:"EMAIL_LINK_BASE_URL" default:""`
	// EmailLogRetentionDays mirrors PostLogRetentionDays: the cleanup_email_logs
	// periodic task drops older rows. 0 disables cleanup entirely.
	EmailLogRetentionDays int `envconfig:"EMAIL_LOG_RETENTION_DAYS" default:"90"`

	// Envelope encryption. KEKPath points at a Docker-volume directory;
	// the actual key file lives at <KEKPath>/kek.v1. The versioned
	// filename leaves room for KEK rotation later without a path
	// migration. Default is a relative ./kek for local dev; the
	// Docker image mounts /var/lib/ogen/keys.
	KEKPath string `envconfig:"OGEN_KEK_PATH" default:"./kek"`

	// Internal operator gRPC surface. Harbor (the ops dashboard) manages the
	// `secret` table through this listener rather than the DB, so it reuses the
	// envelope crypto, allowlist, and hot-reload hooks behind secrets.Store.
	// Internal-only; every call is gated by GRPCAuthToken, a shared bearer token
	// constant-time compared server-side. The server starts ONLY when both
	// GRPCAddr and GRPCAuthToken are set — an empty token means the surface stays
	// off (never run unauthenticated).
	//
	// The default binds LOOPBACK ONLY (127.0.0.1) so a host-run binary never
	// exposes the admin surface on the LAN. To reach it across hosts/containers,
	// set an explicit address (e.g. ":9091" inside a container whose port is
	// published on 127.0.0.1 only — see docker-compose) AND put it behind a
	// firewall / NetworkPolicy / mTLS. The transport itself is plaintext (like
	// Ogen's pdf/video gRPC), so the token must never cross an untrusted network.
	GRPCAddr      string `envconfig:"GRPC_ADDR"       default:"127.0.0.1:9091"`
	GRPCAuthToken string `envconfig:"GRPC_AUTH_TOKEN" default:""`

	// River background-job queue (CON-69 §1, §3; CON-87 WS3). Workers
	// process `submit_post_to_zernio`, `poll_zernio_status`,
	// `cancel_zernio_job`, plus the periodic `reconcile_scheduled_posts`,
	// `cleanup_post_logs`, and `refresh_zernio_analytics`. JobWorkers sizes
	// the worker pool on the default queue; River owns leasing, retry/
	// backoff, and completed-job retention internally.
	JobWorkers int `envconfig:"JOB_WORKERS" default:"4"`
	// Graceful-shutdown wait for in-flight jobs to finish.
	JobShutdownTimeout time.Duration `envconfig:"JOB_SHUTDOWN_TIMEOUT" default:"30s"`

	// Reconciliation grace window (CON-69 §8). A Scheduled post whose
	// scheduled_at + this window has passed without a terminal Zernio
	// status is forced to Failed with a reason that distinguishes
	// reconciliation_timeout from a Zernio-reported failure.
	ReconcileGrace time.Duration `envconfig:"RECONCILE_GRACE" default:"1h"`

	// PostLog retention (CON-69 §11). Older entries are removed by the
	// cleanup_post_logs recurring task. 0 disables cleanup entirely.
	PostLogRetentionDays int `envconfig:"POSTLOG_RETENTION_DAYS" default:"90"`

	// Usage metering & per-tenant cost limits (CON-86). AnalyticsDSN points at
	// the isolated analytics (TimescaleDB) database; empty disables usage
	// recording + enforcement entirely (calls proceed, nothing recorded) and
	// is the graceful-disable default, mirroring empty GeminiAPIKey.
	AnalyticsDSN string `envconfig:"ANALYTICS_DSN" default:""`

	// UsageRetentionDays mirrors PostLogRetentionDays; the analytics-DB
	// retention policy drops vendor_usage_events chunks older than this. (The current
	// migration installs a fixed 90-day policy; operators adjust it out of band.)
	UsageRetentionDays int `envconfig:"USAGE_RETENTION_DAYS" default:"90"`
	// Global default spend caps in USD-micros, applied to any tenant without a
	// tenant_usage_limits row; 0 = no default cap for that period. Mode is
	// enforce (block once over) or warn (record + count, proceed).
	UsageDefaultDailyCapMicros   int64  `envconfig:"USAGE_DEFAULT_DAILY_CAP_MICROS"   default:"0"`
	UsageDefaultMonthlyCapMicros int64  `envconfig:"USAGE_DEFAULT_MONTHLY_CAP_MICROS" default:"0"`
	UsageDefaultMode             string `envconfig:"USAGE_DEFAULT_MODE"               default:"enforce"`

	// Optional JSON override of the in-code per-model price map (mirrors
	// QualityWeightProfiles); empty uses the built-in vendor defaults. (Parsing
	// into the registry is a follow-up; the in-code prices are authoritative.)
	UsageModelPrices string `envconfig:"USAGE_MODEL_PRICES" default:""`

	// UsageAdminToken gates PUT /api/usage/limits — raising a cap or disabling
	// enforcement is an operator action, not tenant self-service, and the user
	// model has no admin/owner role yet (CON-86 §15). A caller must present
	// this value in the X-Admin-Token header. EMPTY (the default) FAILS CLOSED:
	// the write route rejects everyone, so tenants cannot lift their own caps.
	UsageAdminToken string `envconfig:"USAGE_ADMIN_TOKEN" default:""`

	// ActivityRetentionDays mirrors UsageRetentionDays (CON-125): the
	// tenant_activity_events retention policy drops chunks older than this. The current
	// migration installs a fixed 90-day policy; operators adjust it out of band.
	// Starts at 90 days, expandable later.
	ActivityRetentionDays int `envconfig:"ACTIVITY_RETENTION_DAYS" default:"90"`

	// Notification center (CON-242). The cleanup_notifications periodic task
	// reaps faded notifications so the inbox and table stay lean:
	// NotificationsCleanupEvery is the sweep cadence (0 disables the task);
	// NotificationsRetentionDays drops read/dismissed rows older than this (0
	// keeps them — only an explicit expires_at then fades a row). Mirrors
	// PostLogRetentionDays.
	NotificationsCleanupEvery  time.Duration `envconfig:"NOTIFICATIONS_CLEANUP_EVERY"  default:"24h"`
	NotificationsRetentionDays int           `envconfig:"NOTIFICATIONS_RETENTION_DAYS" default:"90"`
}

func Load() (*Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
