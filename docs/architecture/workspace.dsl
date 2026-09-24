workspace "Ogen" "Multi-tenant content operations platform — plan, draft, and auto-publish social content end-to-end." {

    !identifiers hierarchical

    model {
        creator = person "Creator / Operator" "Plans campaigns, drafts posts with AI assistance, and schedules publishing across social platforms."
        admin   = person "Platform Operator" "Manages tenants, secrets, and usage from the ops dashboard."

        ogen = softwareSystem "Ogen" "Plans, drafts, and auto-publishes social content, grounded in each tenant's own content bank." {
            spa = container "Web App" "Content planning and Markdown-first drafting UI. Separate repo, deploys independently." "React + Vite (ogen-app/ui)" "Browser"

            api = container "API" "REST + SSE API, background workers, AI flows, and the internal operator gRPC surface. The core monolith (this repo)." "Go 1.26 · Fiber · Bun · Genkit · River" {
                http     = component "HTTP handlers" "Fiber REST + SSE endpoints; cookie-session auth; per-request tenant resolution." "Fiber / fasthttp"
                workers  = component "River workers" "In-process worker pool: submit/poll/cancel/reconcile posts, analytics + follower refresh, PDF/URL ingestion, email, cleanups." "River"
                genkit   = component "Genkit runtime" "Two long-lived instances: Anthropic (generation/planning/quality, hot-rotatable) and Gemini (embeddings)." "Firebase Genkit"
                grpcSurf = component "Operator gRPC surface" "Internal-only SecretsService + TenantAdminService, bearer-token gated. Consumed by Harbor." "gRPC"
                secrets  = component "Secret store" "Envelope-encrypted secrets (per-secret DEK wrapped by an on-disk KEK); hot rotation without restart." "Go + AES-GCM"
            }

            controlDb = container "Control-plane DB" "Domain data, sessions, encrypted secrets, vector embeddings, and River job tables." "PostgreSQL 17 + pgvector" "Database"
            analyticsDb = container "Analytics DB" "Usage/cost metering, activity events, and follower snapshots — hypertables + continuous aggregates. Isolated from the control plane." "TimescaleDB (PG17)" "Database"
            objectStore = container "Object Storage" "Images, PDFs, video, thumbnails, and poster frames." "S3-compatible (Cloudflare R2 / MinIO)" "Database"

            pdfSvc   = container "pdf-service" "Extracts text, page-aware chunks, and a thumbnail from PDFs. Private network only." "Go + pdfium (ogen-app/pdf-service)"
            videoSvc = container "video-service" "Probes duration/codec/resolution and captures a poster frame. Private network only." "Go + ffmpeg (ogen-app/video-service)"

            harbor  = container "Harbor" "Operator dashboard — tenants, plans, models, secrets, email, announcements, usage. Reads both databases; administers Ogen over the operator gRPC surface." "Go + Next.js (ogen-app/harbor)"
            riverui = container "River UI" "Background-job dashboard; reads River's tables directly." "riverui image"
        }

        # ── External systems ──────────────────────────────────────────────────
        anthropic = softwareSystem "Anthropic Claude API" "Foundation LLM: content planning, the post/campaign assistants, and quality scoring." "External"
        gemini    = softwareSystem "Gemini Embedding API" "Generates vector embeddings for retrieval-augmented grounding." "External"
        zernio    = softwareSystem "Zernio" "Social publishing broker — submit / poll / cancel / reconcile across networks." "External"
        firecrawl = softwareSystem "Firecrawl" "Scrapes a URL to Markdown so a page can be ingested as an asset." "External"
        resend    = softwareSystem "Resend" "Transactional and marketing email delivery." "External"
        sentry    = softwareSystem "Sentry" "Error monitoring and OpenTelemetry trace storage." "External"
        social    = softwareSystem "Social platforms" "LinkedIn, X, Facebook, Instagram, Threads, YouTube." "External"

        # ── Context relationships ────────────────────────────────────────────
        creator -> ogen "Plans, drafts, and schedules content with"
        admin   -> ogen "Administers tenants and secrets in"
        ogen -> anthropic "Generates plans, assistant replies, and quality scores using" "HTTPS"
        ogen -> gemini "Embeds asset chunks with" "HTTPS"
        ogen -> zernio "Submits and reconciles scheduled posts through" "HTTPS"
        zernio -> social "Publishes to"
        ogen -> firecrawl "Scrapes URLs with" "HTTPS"
        ogen -> resend "Sends email through" "HTTPS"
        ogen -> sentry "Reports errors and traces to" "HTTPS / OTLP"

        # ── Container relationships ──────────────────────────────────────────
        creator -> ogen.spa "Plans, drafts, and schedules with" "HTTPS"
        admin   -> ogen.harbor "Administers tenants, plans, models, and secrets with" "HTTPS"

        ogen.spa -> ogen.api "Makes API calls to" "REST + SSE / HTTPS"
        ogen.api -> ogen.controlDb "Reads from and writes to; runs migrations at boot" "SQL (pgx)"
        ogen.api -> ogen.analyticsDb "Records usage/activity and reads aggregates" "SQL (pgx)"
        ogen.api -> ogen.objectStore "Stores and serves media in" "S3 API"
        ogen.api -> ogen.pdfSvc "Parses PDFs via" "gRPC (private)"
        ogen.api -> ogen.videoSvc "Probes video via" "gRPC (private)"
        ogen.api -> anthropic "Generates content with" "HTTPS (Genkit)"
        ogen.api -> gemini "Embeds asset chunks with" "HTTPS (Genkit)"
        ogen.api -> zernio "Submits and reconciles posts through" "HTTPS"
        ogen.api -> firecrawl "Scrapes URLs with" "HTTPS"
        ogen.api -> resend "Sends email through" "HTTPS"
        ogen.api -> sentry "Reports errors and OTel spans to" "HTTPS / OTLP"

        ogen.harbor -> ogen.controlDb "Reads (never migrates)" "SQL"
        ogen.harbor -> ogen.analyticsDb "Reads" "SQL"
        ogen.harbor -> ogen.api "Administers secrets, tenants, plans, models, email, and announcements via the operator surface" "gRPC (private)"
        ogen.api -> ogen.harbor "Notifies of new tenant registrations" "HTTPS webhook (HMAC-signed)"
        ogen.riverui -> ogen.controlDb "Reads River job tables" "SQL"

        # ── Deployment (Railway) ─────────────────────────────────────────────
        railway = deploymentEnvironment "Railway" {
            deploymentNode "Railway project" "" "Railway" {
                deploymentNode "Private network" "" "railway.internal" {
                    apiSvc = containerInstance ogen.api
                    deploymentNode "PostgreSQL 17 + pgvector" {
                        containerInstance ogen.controlDb
                    }
                    deploymentNode "TimescaleDB (PG17)" {
                        containerInstance ogen.analyticsDb
                    }
                    containerInstance ogen.pdfSvc
                    containerInstance ogen.videoSvc
                    containerInstance ogen.harbor
                    containerInstance ogen.riverui
                }
            }
            deploymentNode "Cloudflare R2" "" "S3-compatible object storage" {
                containerInstance ogen.objectStore
            }
            deploymentNode "Static hosting / CDN" "" "SPA deploys independently" {
                containerInstance ogen.spa
            }
        }
    }

    views {
        systemContext ogen "SystemContext" "The people who use Ogen and the external systems it depends on." {
            include *
            autolayout lr
        }

        container ogen "Containers" "The runtime containers that make up Ogen and how they collaborate." {
            include *
            autolayout lr
        }

        component ogen.api "ApiComponents" "Inside the API monolith (C3 — optional, beyond the README's C1/C2 scope)." {
            include *
            autolayout lr
        }

        deployment ogen railway "Deployment" "How the containers map onto Railway infrastructure." {
            include *
            autolayout lr
        }

        styles {
            element "Person" {
                shape person
                background #08427b
                color #ffffff
            }
            element "Software System" {
                background #1168bd
                color #ffffff
            }
            element "External" {
                background #999999
                color #ffffff
            }
            element "Container" {
                background #438dd5
                color #ffffff
            }
            element "Component" {
                background #85bbf0
                color #000000
            }
            element "Database" {
                shape cylinder
            }
            element "Browser" {
                shape webBrowser
            }
        }
    }
}
