# Ogen

[![Codacy Badge](https://app.codacy.com/project/badge/Grade/d5caed68fdc94fb49a1e52ea198d51b7)](https://app.codacy.com/gh/ogen-app/ogen/dashboard?utm_source=gh&utm_medium=referral&utm_content=&utm_campaign=Badge_grade)
[![CI](https://github.com/ogen-app/ogen/actions/workflows/docker-publish.yml/badge.svg)](https://github.com/ogen-app/ogen/actions/workflows/docker-publish.yml)

Ogen is a multi-tenant content operations platform that helps a creator plan,
draft, and publish social content end-to-end:

- **Plan** campaigns and individual posts with AI assistance grounded in your own content bank.
- **Draft** in a Markdown-first editor backed by an LLM assistant that can search assets, rewrite posts, and produce platform-specific variants.
- **Publish** automatically to LinkedIn, X, Facebook, Instagram, Threads, and YouTube via the [Zernio](https://zernio.com) broker — durable background jobs handle submission, polling, cancellation, and reconciliation, with a per-post audit log capturing every state change.

This repository is the **API** — a Go 1.26 monolith (Fiber + Bun + PostgreSQL,
with an in-process [River](https://riverqueue.com) job queue). The React + Vite
SPA lives in its own repository,
[`ogen-app/ui`](https://github.com/ogen-app/ui), and deploys separately.

## Contents

- [Architecture](#architecture)
  - [System context (C4 level 1)](#system-context-c4-level-1)
  - [Containers (C4 level 2)](#containers-c4-level-2)
  - [The API monolith, up close](#the-api-monolith-up-close)
- [Capabilities](#capabilities)
- [AI flows (Genkit)](#ai-flows-genkit)
- [Infrastructure & deployment](#infrastructure--deployment)
- [Development](#development)
- [Backend development](#backend-development)
- [Running the full stack with Docker Compose](#running-the-full-stack-with-docker-compose)
- [Repository skills](#repository-skills-agentsskills)

---

## Architecture

The architecture below is described with the [C4 model](https://c4model.com) at
levels 1 (**System Context**) and 2 (**Containers**). The diagrams are rendered
inline as Mermaid so they show up on GitHub; the canonical model is authored as
a [Structurizr](https://docs.structurizr.com) workspace in
[`docs/architecture/workspace.dsl`](docs/architecture/workspace.dsl) (open it in
[Structurizr Lite](https://docs.structurizr.com/lite) for the full interactive
rendering, including the deployment view).

### System context (C4 level 1)

Ogen is a single software system that a creator drives through the web app and
that an operator administers through Harbor. It depends on a handful of hosted
services for language models, publishing, scraping, and email.

```mermaid
C4Context
    title System Context — Ogen

    Person(creator, "Creator / Operator", "Plans campaigns, drafts posts with AI, schedules publishing")
    Person(admin, "Platform Operator", "Manages tenants, secrets, and usage")

    System(ogen, "Ogen", "Plans, drafts, and auto-publishes social content grounded in each tenant's content bank")

    System_Ext(anthropic, "Anthropic Claude API", "Planning, assistants, quality scoring")
    System_Ext(gemini, "Gemini API", "Embeddings, audio transcription, image vision")
    System_Ext(zernio, "Zernio", "Social publishing broker")
    System_Ext(social, "Social platforms", "LinkedIn · X · Facebook · Instagram · Threads · YouTube")
    System_Ext(firecrawl, "Firecrawl", "URL → Markdown scraping")
    System_Ext(resend, "Resend", "Transactional & marketing email")
    System_Ext(sentry, "Sentry", "Error monitoring + OpenTelemetry traces")

    Rel(creator, ogen, "Plans, drafts, schedules", "HTTPS")
    Rel(admin, ogen, "Administers", "HTTPS / gRPC")
    Rel(ogen, anthropic, "Generates content", "HTTPS")
    Rel(ogen, gemini, "Embeds & analyzes assets", "HTTPS")
    Rel(ogen, zernio, "Submits & reconciles posts", "HTTPS")
    Rel(zernio, social, "Publishes to", "HTTPS")
    Rel(ogen, firecrawl, "Scrapes URLs", "HTTPS")
    Rel(ogen, resend, "Sends email", "HTTPS")
    Rel(ogen, sentry, "Errors & traces", "HTTPS / OTLP")

    UpdateElementStyle(creator, $bgColor="#08427b", $fontColor="#ffffff", $borderColor="#052e56")
    UpdateElementStyle(admin, $bgColor="#08427b", $fontColor="#ffffff", $borderColor="#052e56")
    UpdateElementStyle(ogen, $bgColor="#1168bd", $fontColor="#ffffff", $borderColor="#0b4884")
    UpdateElementStyle(anthropic, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(gemini, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(zernio, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(social, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(firecrawl, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(resend, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(sentry, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateRelStyle(creator, ogen, $textColor="#444444", $lineColor="#707070")
    UpdateRelStyle(admin, ogen, $textColor="#444444", $lineColor="#707070")

    UpdateLayoutConfig($c4ShapeInRow="3", $c4BoundaryInRow="1")
```

### Containers (C4 level 2)

Inside the Ogen boundary, the **API** monolith is the hub: it serves the SPA over
REST + SSE, owns the control-plane and analytics databases, drains its own
in-process River worker pool, runs the Genkit AI flows, and exposes an
internal-only gRPC surface that Harbor uses to manage secrets, tenants, plans,
the platform catalog, per-flow model assignments, email, and announcements. PDF, video, document, audio, and image processing are
split out into private-network sidecar services.

```mermaid
C4Container
    title Container diagram — Ogen

    Person(creator, "Creator / Operator", "Plans, drafts, schedules")
    Person(admin, "Platform Operator", "Tenants, secrets, usage")

    System_Boundary(ogen, "Ogen") {
        Container(spa, "Web App", "React + Vite (ogen-app/ui)", "Planning & Markdown-first drafting UI; separate deploy")
        Container(api, "API", "Go 1.26 · Fiber · Bun · Genkit · River", "REST + SSE, background workers, AI flows, operator gRPC surface")
        ContainerDb(pg, "Control-plane DB", "PostgreSQL 17 + pgvector", "Domain data, sessions, encrypted secrets, embeddings, River job tables")
        ContainerDb(ts, "Analytics DB", "TimescaleDB (PG17)", "Usage/cost, activity, follower snapshots — hypertables + continuous aggregates")
        ContainerDb(obj, "Object Storage", "S3-compatible (R2 / MinIO)", "Images, PDFs, audio, video, thumbnails, posters")
        Container(pdf, "pdf-service", "Go + pdfium", "PDF text, page-aware chunks, thumbnail — private network")
        Container(video, "video-service", "Go + ffmpeg", "Video probe + poster frame — private network")
        Container(doc, "document-service", "Go (pure)", "Office/text docs → source-anchored chunks — private network")
        Container(audio, "audio-service", "Go + ffmpeg + Gemini", "Audio transcode + transcribe → time-anchored transcript — private network")
        Container(image, "image-service", "Go + libvips + Gemini", "Image normalize + EXIF-strip + vision extraction (classify/describe/alt) → anchored chunks — private network")
        Container(harbor, "Harbor", "Go + Next.js (ogen-app/harbor)", "Ops dashboard: tenants, plans, models, secrets, email, usage")
        Container(riverui, "River UI", "riverui image", "Background-job dashboard")
    }

    System_Ext(anthropic, "Anthropic Claude API", "LLM")
    System_Ext(gemini, "Gemini API", "Embeddings, audio transcription, image vision")
    System_Ext(zernio, "Zernio", "Publishing broker")
    System_Ext(firecrawl, "Firecrawl", "Scraping")
    System_Ext(resend, "Resend", "Email")
    System_Ext(sentry, "Sentry", "Errors + traces")

    Rel(creator, spa, "Uses", "HTTPS")
    Rel(admin, harbor, "Uses", "HTTPS")
    Rel(spa, api, "REST + SSE", "HTTPS")

    Rel(api, pg, "Reads/writes; migrates at boot", "SQL / pgx")
    Rel(api, ts, "Records & reads", "SQL / pgx")
    Rel(api, obj, "Stores & serves media", "S3")
    Rel(api, pdf, "Parses PDFs", "gRPC")
    Rel(api, video, "Probes video", "gRPC")
    Rel(api, doc, "Parses documents", "gRPC")
    Rel(api, audio, "Transcribes audio", "gRPC")
    Rel(api, image, "Ingests images", "gRPC")

    Rel(api, anthropic, "Generation / planning / quality", "HTTPS")
    Rel(api, gemini, "Embeddings", "HTTPS")
    Rel(audio, gemini, "Transcribes audio (multimodal)", "HTTPS")
    Rel(image, gemini, "Vision: classify / extract / alt", "HTTPS")
    Rel(api, zernio, "Submit / poll / cancel / reconcile", "HTTPS")
    Rel(api, firecrawl, "Scrape URL", "HTTPS")
    Rel(api, resend, "Send email", "HTTPS")
    Rel(api, sentry, "Errors & OTel spans", "HTTPS / OTLP")

    Rel(harbor, pg, "Reads", "SQL")
    Rel(harbor, ts, "Reads", "SQL")
    Rel(harbor, api, "Operator admin", "gRPC")
    Rel(api, harbor, "New-tenant webhook", "HTTPS")
    Rel(riverui, pg, "Reads job tables", "SQL")

    UpdateElementStyle(creator, $bgColor="#08427b", $fontColor="#ffffff", $borderColor="#052e56")
    UpdateElementStyle(admin, $bgColor="#08427b", $fontColor="#ffffff", $borderColor="#052e56")
    UpdateElementStyle(spa, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(api, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(pg, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(ts, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(obj, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(pdf, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(video, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(doc, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(audio, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(image, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(harbor, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(riverui, $bgColor="#438dd5", $fontColor="#ffffff", $borderColor="#2e6295")
    UpdateElementStyle(anthropic, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(gemini, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(zernio, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(firecrawl, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(resend, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")
    UpdateElementStyle(sentry, $bgColor="#999999", $fontColor="#ffffff", $borderColor="#6b6b6b")

    UpdateLayoutConfig($c4ShapeInRow="3", $c4BoundaryInRow="1")
```

### The API monolith, up close

The API is a layered Go monolith. A single boot sequence
(`cmd/server/main.go`) loads config, connects and migrates both databases,
initialises the envelope-encryption cipher + secret store, wires the server, and
starts the HTTP listener alongside the internal gRPC listener.

```
cmd/server/        # main entrypoint: config → DB + migrations → secrets → server.New → listen

src/
  transport/                       # inbound surface (CON-291 role grouping)
    handlers/                      # Fiber HTTP handlers (REST + SSE) with swag annotations
    grpc/server/                   # internal operator gRPC surface (Secrets, TenantAdmin, PlanAdmin, PlatformAdmin, ModelConfigAdmin, EmailAdmin, AnnouncementAdmin)
    grpc/client/{pdf,video,documents,audio,image}/ # gRPC clients we consume (pdf/video/document/audio/image sidecars)
    server/                        # server.New: wires repos → handlers → workers → routes; Genkit runtime
  usecase/                         # application orchestration / services — the one home for "where orchestration lives"
    post_actions/ campaign_actions/ scheduling/ campaigngoal/ notes/ settings/
    brandresolve/ notify/ accountselect/ tenant_actions/ activity/
  domain/                          # business core
    models/                        # bun-tagged record structs (single source of truth for the schema)
    platforms/                     # per-platform attachment + text validation rules, Markdown → social-text flattening
    entitlements/                  # tier entitlements resolver + nil-safe, fail-open Limiter (caps & gates)
    modelconfig/                   # flow/slot catalog + tier-aware model resolver
  infra/                           # persistence + outward adapters
    repository/                    # narrow per-aggregate persistence layer
    database/                      # bun.DB factories (control-plane + analytics) and migrations
    publishers/                    # Publisher abstraction; publishers/zernio is the concrete impl
    storage/ secrets/ crypto/ email/ firecrawl/ embedding/ eventhub/ vendors/
  kernel/                          # cross-cutting: config/ logging/ telemetry/ tenantctx/ netguard/ usage/ activity/
  jobs/ jobs/queues/               # River runtime + workers (submit/poll/cancel/reconcile, analytics, PDF/URL, email)
  genkit/                          # Genkit AI flows (assistants, content plan, quality, embeddings)
  analytics/                       # post-analytics read models (overview / performers / lessons / per-post)
  pgtest/ integration/             # test harness (real Postgres) + //go:build integration suite
```

**Persistence.** Domain data, sessions, the encrypted `secret` table, vector
embeddings (`assets_chunks.embedding` as a pgvector `halfvec(3072)`), and River's
own job tables all live in the **control-plane PostgreSQL 17 + pgvector**
database. Bun is the ORM; migrations under `src/infra/database/migrations/` run on
every boot. Because River shares the same pool, background-job state, Post Log
entries, and domain rows can co-commit in one transaction (used for the
auto-publish schedule decision). A **separate TimescaleDB** database
(`ANALYTICS_DSN`) holds the metering and analytics hypertables + continuous
aggregates, kept isolated so the extension dependency stays quarantined and the
control plane stays vanilla. Analytics is fail-open: a connect/migrate failure at
boot disables it rather than taking down the API.

**Background jobs.** [River](https://riverqueue.com) is Postgres-native — no
external broker — and owns leasing, retry/backoff, periodic scheduling, and
completed-job retention. Workers process `submit_post_to_zernio`,
`poll_zernio_status`, `cancel_zernio_job`, `process_pdf`, `process_document`,
`process_url`, `process_audio` (on a dedicated `audio` queue so long
transcriptions can't starve short ingestion), `process_image` (likewise on a
dedicated `image` queue so heavy vision runs can't starve short ingestion),
`send_email`, `bootstrap_zernio_profile` / `teardown_zernio_profile` (per-workspace
Zernio profile on signup / workspace delete), `notify_harbor_tenant_registered`,
plus periodic `reconcile_scheduled_posts`,
`refresh_zernio_analytics`, `refresh_zernio_followers`,
`detect_expiring_connections`, `detect_manual_publish_due`, and the `cleanup_*`
sweepers. The
[River UI](https://riverqueue.com/ui) dashboard (a standalone container) reads
those tables directly.

---

## Capabilities

| Area | What's there |
|------|--------------|
| **Multi-tenancy** | Public SaaS signup mints a tenant; all domain rows are tenant-scoped and resolved per request. Users, sessions, invitations, and password reset are first-class. |
| **Campaigns & posts** | Campaign types, campaigns, posts, post versions, notes, per-platform validation, quality scoring, and streaming AI assistants. |
| **Content bank** | Markdown notes, PDF, office/text document, audio (transcribed to time-anchored chunks), image (vision-extracted to source-anchored blocks + alt text), and URL assets (scraped via Firecrawl), with paragraph-/source-aware chunking, Gemini embeddings, and pgvector similarity search used to ground the assistants. Document, audio, and image ingestion run in the `document-service` / `audio-service` / `image-service` sidecars. |
| **Attachments** | Image, PDF, and video uploads on S3-compatible storage, processed by the `image-service` / `pdf-service` / `video-service` sidecars (EXIF/geo-strip + alt text, thumbnails, page counts, duration/codec/poster). Per-platform constraints surface as soft validation warnings. |
| **Zernio auto-publish** | `Publisher` abstraction with Zernio as the concrete impl. Submit / poll / cancel / retry are River jobs; a reconciliation sweeper guards against stuck `Scheduled` posts. Headless account connect avoids Zernio's hosted picker. Native threads on X and Threads; Markdown is flattened to platform text at egress and limits count visible length. Deleting a workspace tears down its Zernio profile. |
| **Post Log** | Auditable per-post history — state transitions, validation outcomes, background-task lifecycle, Zernio interactions, reconciliation timeouts, user actions. Sanitized of secrets, size-capped, configurable retention. |
| **Analytics** | Cumulative overview KPIs, best/worst performers with age-adjusted "against typical", all-time lessons (heatmap + lifespan curve), and per-post drill-down — backed by TimescaleDB continuous aggregates. |
| **Activity & notifications** | Durable per-user notification inbox (REST + SSE with `Last-Event-ID` replay), server-side Activity daily reports, and producers such as expiring connections and manual-publish-due posts. |
| **Announcements** | Operator-authored in-app banners, targeted to all tenants or by tier or group, with per-user click/dismiss tracking. |
| **Plans & entitlements** | Versioned tier entitlements (Trial / Pro / Max). A fail-open `Limiter` enforces standing caps (seats, campaigns, assets, storage, web imports → 402) and campaign-type gates (→ 403), and sends near-limit notifications. Live usage counts are returned by `GET /api/me/entitlements`. |
| **Usage metering** | Per-tenant model/publisher cost metering with optional daily/monthly spend caps (enforce or warn), recorded to the analytics DB via a pluggable vendor registry. |
| **Brand materials** | Voices, audiences, and guardrails bound to campaigns/posts and injected into the writing flows (precedence: post → campaign → default). |
| **Email** | Transactional + drip email via Resend, with embedded templates, one-click unsubscribe, and delivery webhooks. Delivery events (delivered/opened/clicked/bounced…) and rendered bodies are persisted for the Harbor Emails tab. New-tenant signups notify operators via a signed Harbor webhook. |
| **Secrets** | Envelope-encrypted at rest (per-secret DEK wrapped by an on-disk KEK). Rotatable via the operator gRPC surface without restart. |
| **Operator surface** | Internal gRPC `SecretsService`, `TenantAdminService`, `PlanAdminService`, `PlatformAdminService`, `ModelConfigAdminService`, `EmailAdminService`, and `AnnouncementAdminService`, consumed by the Harbor ops dashboard over the private network. |
| **Observability** | Sentry error monitoring + OpenTelemetry tracing (HTTP → Genkit → model → DB → gRPC → River), continuing the UI's `sentry-trace`. Fail-open: disabled unless `SENTRY_DSN` is set. |

---

## AI flows (Genkit)

All AI-powered features are [Firebase Genkit](https://firebase.google.com/docs/genkit)
flows under `src/genkit/flows/`. Two long-lived Genkit instances run at boot:

- **Embedding instance** — the `googlegenai` plugin (Gemini Embedding 2). The wrapper is stable for the process lifetime; its backing embedder is rebuilt when `gemini_api_key` is set or rotated, so the key rotates without restart.
- **Anthropic instance** — owned by `gkRuntime` (`src/transport/server/genkit_runtime.go`). Hot-rebuildable, so rotating `anthropic_api_key` re-registers all Anthropic flows without a restart. With no key the runtime stays nil and consuming handlers return 503.

Models are configured **per flow and per slot**, and are never hardcoded
(`src/domain/modelconfig`). Each flow declares one or more model *slots*, and
each slot sets the capabilities a model must have (tools, structured output,
streaming, minimum output/context size, embedding dimensions). Assignments live
in the `flow_model_config` table. They are resolved at call time, and a
tier-specific row wins over the global default. Operators edit them from Harbor
via `ModelConfigAdminService`. A write is rejected when the model doesn't meet
the slot's requirements, and it can be checked first with a live probe. On boot,
missing global-default rows are seeded from the legacy `MODEL_ID` /
`PLANNING_MODEL_ID` / `QUALITY_MODEL_ID` / `EMBED_MODEL` env vars (now
**seed-only**; after that, the DB is authoritative).

| Flow | Slot(s) | Seed default |
|------|---------|--------------|
| `content_plan`, `draft_post`, `enrich_brief`, `consistency` | `main` | Claude Sonnet 4.5 (`MODEL_ID`) |
| `post_quality` | `main` | Claude Sonnet 4.5 (`QUALITY_MODEL_ID`) |
| `post_assistant` | `planner` / `writer` | Claude Haiku 4.5 (`PLANNING_MODEL_ID`) / Sonnet 4.5 (`MODEL_ID`) |
| `campaign_assistant` | `orchestrator` | Claude Haiku 4.5 (`PLANNING_MODEL_ID`) |
| `embed` | `main` (global only, no tier override) | Gemini Embedding 2 (`EMBED_MODEL`, 3072-dim) |

The sidecars choose their own Gemini models (these aren't in the resolver yet):

| Use | Model | Where |
|-----|-------|-------|
| transcription | Gemini 2.5 Flash (`TRANSCRIBE_MODEL`, multimodal) | `audio-service` segment transcription (priced via the `gemini` vendor) |
| vision | Gemini 2.5 Flash / 2.5 Pro (`VISION_CLASSIFY_MODEL` / `VISION_EXTRACT_MODEL`) | `image-service` classify → extract → describe → alt text (priced via the `gemini` vendor) |

| Flow | Purpose | Triggered by |
|------|---------|--------------|
| `embed_asset` | Chunks an asset's Markdown (paragraph-aware, with overlap) and writes embeddings into `assets_chunks`. | Asset save callback. |
| `process_pdf` (River) | Calls `pdf-service` for text + page-aware chunks + thumbnail, uploads to S3, then chunks + embeds. | PDF upload. |
| `process_url` (River) | Scrapes a URL to Markdown via Firecrawl, mirrors images to S3, stores it as a URL asset, then chunks + embeds. | URL asset creation. |
| `process_document` (River) | Calls `document-service` for office/text docs (docx/pptx/xlsx/odt/epub/csv/html/eml/…) → source-anchored chunks, then embeds. | Document upload. |
| `process_audio` (River, dedicated `audio` queue) | Probes + tier-gates, normalizes once, segments, transcribes each segment via `audio-service` (Gemini multimodal), assembles time-anchored transcript chunks, then embeds. Checkpointed + resumable. | Audio upload / finalize. |
| `process_image` (River, dedicated `image` queue) | Presigns the original + a normalized slot, runs the full `image-service` pipeline (normalize + EXIF-strip + classify + per-shape extraction + description + alt text), persists the extraction/blocks, embeds the searchable text, and snapshots cost. | Image asset upload. |
| `content_plan` | Per-phase, per-platform post drafts for a campaign; parallel K-sized batches against Anthropic with SSE progress. | `POST /api/campaigns/:id/generate-draft` (SSE). |
| `post_assistant` | Interactive post editor. Haiku planner loop + Sonnet `editPost` write-tool; streams `explanation_delta` / `content_delta`; tool use over the asset library. | `POST /api/posts/:id/assistant` (SSE). |
| `campaign_assistant` | Campaign-level planning assistant; flows-as-tools (content plan, enrich brief, draft post, consistency check) under a Haiku orchestration loop. | Campaign assistant endpoint (SSE). |
| `post_quality` | Scores a post version across weighted per-platform dimensions. | Quality assessment. |

---

## Infrastructure & deployment

Ogen runs as a small fleet of containers. In production they are deployed on
[Railway](https://railway.app); the sidecar services are reached **only** over
Railway's private network (`*.railway.internal`), never a public port.

**Repository family**

| Repo | Role |
|------|------|
| [`ogen-app/ogen`](https://github.com/ogen-app/ogen) | API monolith (this repo). |
| [`ogen-app/ui`](https://github.com/ogen-app/ui) | React + Vite SPA; deploys independently. |
| [`ogen-app/harbor`](https://github.com/ogen-app/harbor) | Operator dashboard (Go backend + Next.js UI). |
| [`ogen-app/pdf-service`](https://github.com/ogen-app/pdf-service) | PDF parsing sidecar (CGO + pdfium). |
| [`ogen-app/video-service`](https://github.com/ogen-app/video-service) | Video probing sidecar (ffmpeg/ffprobe). |
| [`ogen-app/document-service`](https://github.com/ogen-app/document-service) | Office/text document parsing sidecar (pure Go). |
| [`ogen-app/audio-service`](https://github.com/ogen-app/audio-service) | Audio transcode + transcribe sidecar (ffmpeg + Gemini multimodal). |
| [`ogen-app/image-service`](https://github.com/ogen-app/image-service) | Image normalize + vision-extraction sidecar (libvips/CGO + Gemini vision). |
| `buf.build/ogen-app/proto` | Shared gRPC contracts (tenants, secrets, plans, platforms, modelconfig, email, announcements, pdf, video, documents, audio, image), published as a [BSR](https://buf.build) module and pinned via `make proto`. |

**Runtime topology**

- **API** — stateless Go binary (static, `CGO_ENABLED=0`, Alpine runtime). Serves HTTP (`ADDR`, `:3000` in the image) and the internal gRPC surface (`GRPC_ADDR`, bind `:9091` in a container so Harbor can reach it; loopback-only by default). Drops from root to an unprivileged user at entry after fixing KEK-volume permissions.
- **Control-plane database** — PostgreSQL 17 + the `vector` extension (`pgvector/pgvector:pg17`).
- **Analytics database** — `timescale/timescaledb:latest-pg17`, separate instance (`ANALYTICS_DSN`).
- **Object storage** — Cloudflare R2 in production, MinIO locally (any S3-compatible endpoint via `STORAGE_*`).
- **pdf-service / video-service / document-service / audio-service / image-service** — gRPC on `:50051`, private network only. audio-service and image-service additionally call the Gemini Developer API — for transcription and vision extraction, respectively (both share `gemini_api_key`).
- **Harbor** — reads both databases and calls the API's operator gRPC surface; shares the control-plane Postgres server via its own `harbor` database.
- **River UI** — job dashboard reading the control-plane database.

**Secrets & keys.** API keys (Anthropic, Zernio, Gemini, Firecrawl, Resend,
webhook + link-signing secrets) are **not** required as env vars. They are seeded
from the environment on first boot into the envelope-encrypted `secret` table and
thereafter read from — and rotated via — the operator gRPC surface, with no
restart. The **KEK** (envelope-encryption master key) lives at
`<OGEN_KEK_PATH>/kek.v1` on a persistent volume (`/var/lib/ogen/keys` in the
image). **Back it up** — losing it bricks every encrypted secret. The gRPC
transport is plaintext + bearer-token gated, so it must never cross an untrusted
network.

The Structurizr [deployment view](docs/architecture/workspace.dsl) renders this
topology onto Railway (open the workspace in Structurizr Lite).

---

## Development

Two workflows, depending on what you're changing:

| You're working on | Use this workflow | What you need |
|-------------------|-------------------|---------------|
| Frontend only | The **[`ogen-app/ui`](https://github.com/ogen-app/ui)** repo — its Vite dev server proxies `/api` to a locally-running API. | Node + pnpm (see that repo). |
| Backend (`src/`, `cmd/`) | **[Backend development](#backend-development)** — build the API from source; point the UI at `http://localhost:9001`. | Go 1.26 + Docker. |

The UI targets `http://localhost:9001` by default via its Vite `/api` proxy, so a
locally-running API is picked up automatically.

## Backend development

### Prerequisites

| Tool | Minimum | Notes |
|------|---------|-------|
| Go | 1.26 | The module sets `go 1.26.1`. |
| Docker Desktop | 4.x | PostgreSQL, TimescaleDB, MinIO, the sidecars, and the unit/integration suites all run in containers. |
| `make` | any | Build / test / openapi / proto targets. |
| `buf` | any | Only needed to regenerate gRPC stubs (`make proto`); generated code is committed. |

There is **no** local PDF/video/audio/image toolchain to install — parsing,
transcription, and vision extraction moved to the `pdf-service` /
`video-service` / `document-service` / `audio-service` / `image-service`
sidecars (gRPC). When a sidecar is absent the API degrades gracefully: PDF/video
uploads are accepted but unprobed, while document, audio, and image uploads fail
fast with a clear "not configured" message.

### 1. Clone and pull deps

```bash
git clone https://github.com/ogen-app/ogen.git
cd ogen
go mod download
```

### 2. Start the databases

The default `DATABASE_DSN` points at `postgres://ogen:ogen@localhost:5432/ogen`.
Bring up Postgres (and the analytics TimescaleDB) via Docker Compose:

```bash
docker compose up postgres timescaledb -d
```

Migrations for both databases run automatically on every boot.

### 3. KEK (envelope-encryption master key)

The first run generates one under `OGEN_KEK_PATH` (default `./kek/kek.v1`).
**Back this file up** — losing it bricks every encrypted secret. In deployment,
mount it from a persistent volume.

### 4. (Optional) seed the Gemini embedding key

Embeddings + semantic search use the hosted **Gemini Embedding 2** API. Set
`GEMINI_API_KEY` to seed it on first boot (thereafter rotate it via the operator
surface). With no key the server still boots: asset saves succeed, but no vectors
are written and semantic search returns nothing.

```bash
export GEMINI_API_KEY=AIza...   # optional; enables embedding + RAG
```

### 5. Run with hot reload

```bash
make run    # uses `air` for live rebuilds on .go changes
```

The API listens on `http://localhost:9001`. Or build + run once:

```bash
make build && ./server
```

### 6. Configure secrets at runtime

You don't have to set `ANTHROPIC_API_KEY` / `ZERNIO_API_KEY` / etc. as env vars —
the server boots without them and surfaces an "integration disabled" state. Seed
them via env on first boot, or manage them through Harbor's Settings → Secrets tab
(which talks to the API's operator gRPC surface). Both are encrypted at rest with
a per-secret DEK wrapped by the KEK, and rotate without restart.

### Common Make targets

```bash
make build            # build the server binary into ./server
make run              # air-powered hot reload for the API
make genkit           # boot the API under the Genkit dev UI (:4000)
make test             # ginkgo unit/handler suites against a throwaway Postgres (race, coverage)
make test-integration # spins up MinIO + Postgres, runs //go:build integration specs
make openapi          # regenerate docs/swagger.json from swag annotations
make proto            # regenerate gen/ gRPC stubs from buf.build/ogen-app/proto
make tidy             # go mod tidy
make docker           # build the API Docker image
```

### Configuration

All runtime knobs are env vars, loaded by
[`envconfig`](https://github.com/kelseyhightower/envconfig). See
`src/kernel/config/config.go` for the full, documented list. Highlights:

| Variable | Default | Purpose |
|----------|---------|---------|
| `ADDR` | `:9001` | HTTP listen address (the Docker image sets `:3000`). |
| `DATABASE_DSN` | `postgres://ogen:ogen@localhost:5432/ogen?sslmode=disable` | Control-plane Postgres (pgvector). |
| `ANALYTICS_DSN` | empty | Isolated analytics TimescaleDB. Empty disables usage metering + analytics (fail-open). |
| `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS` | 25 / 5 | Shared pool sizing for HTTP + River workers. |
| `GEMINI_API_KEY` | empty | Gemini Embedding 2 key — first-boot seed; rotate via the operator surface. Empty disables embedding/RAG. |
| `EMBED_MODEL` / `EMBED_DIMENSIONS` | `gemini-embedding-2` / `3072` | Embedding model + vector width (must match `assets_chunks.embedding`). |
| `ANTHROPIC_API_KEY` / `MODEL_ID` | empty / `claude-sonnet-4-5-20250929` | Claude key (first-boot seed) + generation model (**seed-only**; see [AI flows](#ai-flows-genkit)). |
| `PLANNING_MODEL_ID` / `QUALITY_MODEL_ID` | Haiku 4.5 / Sonnet 4.5 | Routing + quality-scoring models (**seed-only**; Harbor-managed after first boot). |
| `STORAGE_*` | empty | S3-compatible storage (R2/MinIO/AWS); empty disables uploads. |
| `PDF_SERVICE_ADDR` / `VIDEO_SERVICE_ADDR` / `DOCUMENTS_SERVICE_ADDR` / `AUDIO_SERVICE_ADDR` / `IMAGE_SERVICE_ADDR` | empty | gRPC sidecar addresses (`*.railway.internal:50051` in prod). Empty disables that pipeline. |
| `ZERNIO_API_KEY` / `ZERNIO_BASE_URL` | empty / `https://zernio.com/api/v1` | Zernio integration; key resolved per call, so rotations land without restart. |
| `FIRECRAWL_API_KEY` | empty | URL-asset scraping; empty disables URL ingestion. |
| `RESEND_API_KEY` / `EMAIL_FROM` | empty / `Ogen <hello@getogen.com>` | Email delivery; empty disables sending. |
| `OGEN_KEK_PATH` | `./kek` | Directory holding `kek.v1` — losing it bricks every encrypted secret. |
| `GRPC_ADDR` / `GRPC_AUTH_TOKEN` | `127.0.0.1:9091` / empty | Internal operator gRPC surface; starts only when both are set. |
| `HARBOR_WEBHOOK_URL` / `HARBOR_WEBHOOK_SECRET` / `HARBOR_BASE_URL` | empty | New-tenant operator notification: Harbor inbound webhook, HMAC signing key, and "View in Harbor" deep-link base. Empty URL turns the feature off. |
| `SENTRY_DSN` | empty | Turns on error monitoring + OTel tracing; empty disables all telemetry. Also `SENTRY_ENVIRONMENT`, `SENTRY_RELEASE`, `SENTRY_TRACES_SAMPLE_RATE` (`0.1`), `OTEL_SERVICE_NAME` (`ogen-api`). |
| `JOB_WORKERS` / `JOB_SHUTDOWN_TIMEOUT` | 4 / 30s | River worker pool + graceful-shutdown wait. |
| `RECONCILE_GRACE` | `1h` | Reconciliation timeout for stuck Scheduled posts. |
| `POSTLOG_RETENTION_DAYS` | `90` | Post Log retention window. |

### Testing

The unit/handler suite runs against a **real, throwaway Postgres** (no mocks) —
`make test` boots a disposable `pgvector` container and every test provisions its
own database through the migration chain. The integration suite
(`//go:build integration`) brings up MinIO + Postgres via
`docker-compose.integration.yml`; the embedding + chunking specs additionally hit
the live Gemini API and skip unless `GEMINI_API_KEY` is set.

```bash
make test                  # unit/handler (throwaway Postgres, race + coverage)
make test-integration      # MinIO + Postgres (slow, docker compose)
```

### Observability

| Route | Purpose |
|-------|---------|
| `/api/health` | Liveness/readiness. |
| `/debug/vars` | `expvar` JSON — `ogen_jobs_*` counters and Zernio API latency (behind session auth). |
| `/api/posts/:id/log` · `/api/post-logs` | Per-post and cross-post audit history. |
| River UI (`:9004` in compose) | Running / upcoming / completed / failed jobs per queue. |
| Sentry (when `SENTRY_DSN` is set) | Grouped panics/5xx and end-to-end OpenTelemetry traces (HTTP → Genkit → model → DB → gRPC), continued from the UI's `sentry-trace` header. See the `opentelemetry-tracing` skill. |

The full REST reference (OpenAPI) is generated from swag annotations via
`make openapi` and published from `docs/swagger.json` on every merge to `main`.

---

## Running the full stack with Docker Compose

`docker-compose.yml` runs the API plus its dependencies; optional services are
gated behind [profiles](https://docs.docker.com/compose/profiles/) so the default
`docker compose up` stays backend-only.

```bash
docker compose up                      # API + Postgres + TimescaleDB + pdf-service (+ River UI on :9004)
docker compose --profile ui up         # + the ogen-app/ui Vite dev server on :9002
docker compose --profile video up      # + video-service
docker compose --profile documents up  # + document-service
docker compose --profile audio up      # + audio-service
docker compose --profile image up      # + image-service
docker compose --profile harbor up     # + Harbor backend (:9003) & UI (:9005)
```

| Service | URL | Notes |
|---------|-----|-------|
| API | http://localhost:9001 | Set `CORS_ALLOWED_ORIGINS` when the UI is on a different origin. |
| UI dev server (`ui` profile) | http://localhost:9002 | Mounts a sibling `../ui` checkout; override with `UI_PATH`. |
| River UI | http://localhost:9004 | Reads River's job tables. |
| Harbor backend (`harbor` profile) | http://localhost:9003 | Shares this stack's Postgres (its own `harbor` DB) + the operator gRPC surface at `api:9091`. |
| Harbor UI (`harbor` profile) | http://localhost:9005 | Next.js dev server; override the checkout with `HARBOR_PATH`. |

Copy `.env.example` → `.env` and fill it in before `docker compose up`; all
configuration (including seed secrets) is read from there.

---

## Repository skills (`.agents/skills/`)

The `.agents/skills/` directory ships **scaffolding skills** for
[Claude Code](https://claude.ai/code) (and any agent harness honouring the
`SKILL.md` convention). Each encodes the project's existing patterns so a new
feature lands looking like the rest of the codebase.

| Skill | When to invoke | What it scaffolds |
|-------|----------------|-------------------|
| **`add-entity`** | Adding a new resource / table / domain entity — a full vertical slice. | Migration up/down, `bun`-tagged model, narrow `Repository` + impl, Fiber handler with swag annotations, `server.New` wiring, ginkgo tests against real Postgres, and an `http-client/` example. |
| **`add-genkit-flow`** | Adding an AI assistant / generator / classifier — anything LLM-driven. | A flow package mirroring `post_assistant` / `content_plan`: types, prompt template, `run.go` with the SSE-streaming closure, optional tools, context cache, runtime init, streaming handler, and tests. |
| **`add-river-job`** | Adding a background/async task, queue worker, or recurring sweep. | River args type, worker/processor, self-registration, transactional enqueue, periodic scheduling, and tests. |
| **`add-grpc-service`** | Adding a gRPC service/client or a private-network sidecar. | buf-managed proto codegen, (client-)streaming RPCs, health service, graceful shutdown, status-error mapping, and a nil-disabled thin client — the `pdf-service` pattern. |
| **`email-templates`** | Adding/editing an email, its copy/design, or the drip schedule. | The Maizzle authoring project + DB seeds + embedded-default → seed-on-boot → DB-wins lifecycle, the `[[ ]]` Go delimiters, and the brand tokens (CON-154). |
| **`opentelemetry-tracing`** | Wiring, debugging, or extending tracing / Sentry — missing or unparented spans, broken `sentry-trace` / `traceparent` propagation, sampling. | The `telemetry.Init` wiring point, Sentry-via-OTLP export, the sampler + propagator, and the Fiber `c.Context()` vs `c.UserContext()` span-parenting trap. |

All follow the project's conventions — real Postgres tests at the handler layer
(no mocks), authz-before-lookup with 403-over-404, `feature/` branch naming — and
refuse to invent new patterns where canonical references already exist. The
`SKILL.md` files are themselves the spec; read them to audit the patterns the
project considers canonical.

```bash
# Inside Claude Code, or any agent harness that loads .agents/skills:
/skill add-entity         # then describe the entity
/skill add-genkit-flow    # then describe the flow
```