# syntax=docker/dockerfile:1
# The React SPA lives in its own repo (ogen-app/ui) and deploys separately
# (CON-98). This image builds the API only.
# ─── Stage 1: build Go binary ────────────────────────────────────────────────
FROM golang:1.26-alpine AS go-builder

WORKDIR /app

# Download Go modules first — this layer is cached as long as go.mod/go.sum are
# unchanged. (No BuildKit cache mounts below: Railway's builder requires cache-
# mount ids to be prefixed with a hardcoded `s/<service-id>-`, which would couple
# this Dockerfile to a single Railway service and break local / CI / prod builds.)
COPY go.mod go.sum ./
RUN go mod download

# Copy source.
COPY . .

# Build a static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -p 4 -trimpath -ldflags="-s -w" -o /server ./cmd/server

# ─── Stage 2: GeoIP database ─────────────────────────────────────────────────
# DB-IP "IP to City Lite" (CC BY 4.0, no licence key) for the location line in
# new-device login alerts. Built into the image so every environment built from
# this Dockerfile — Railway staging from source, the published production image,
# local `docker build` — has it. DB-IP publishes one file per month, and the
# current month's file can lag its first days, so the previous month is the
# fallback. Pin a month with --build-arg GEOIP_MONTH=YYYY-MM (on Railway, a
# service variable of that name); a changed value also refreshes a cached layer.
# The build fails if neither file downloads, so a deploy never silently ships
# without the database.
FROM alpine:3.20 AS geoip
ARG GEOIP_MONTH=
RUN apk add --no-cache curl && \
    this="${GEOIP_MONTH:-$(date -u +%Y-%m)}" && \
    day="$(date -u +%d)" && \
    prev="$(date -u -d "@$(( $(date -u +%s) - ${day#0} * 86400 ))" +%Y-%m)" && \
    for m in "$this" "$prev"; do \
      curl -fsSL --retry 3 -o /tmp/city.mmdb.gz "https://download.db-ip.com/free/dbip-city-lite-${m}.mmdb.gz" && break; \
      rm -f /tmp/city.mmdb.gz; \
    done && \
    test -s /tmp/city.mmdb.gz && \
    gunzip -t /tmp/city.mmdb.gz && \
    mkdir -p /geoip && gunzip -c /tmp/city.mmdb.gz > /geoip/dbip-city-lite.mmdb

# ─── Stage 3: Alpine runtime ─────────────────────────────────────────────────
# Alpine (not scratch) for ca-certificates/tzdata and su-exec. PDF parsing,
# thumbnails, and page counting moved to the pdf-service microservice (CON-103),
# so poppler-utils (pdftotext/pdftoppm) is no longer installed here.
FROM alpine:3.20

# su-exec lets the entrypoint drop from root to appuser after fixing volume perms.
RUN apk add --no-cache ca-certificates tzdata su-exec && \
    addgroup -S appgroup && adduser -S -G appgroup appuser && \
    mkdir -p /var/lib/ogen/keys && \
    chown appuser:appgroup /var/lib/ogen/keys && \
    chmod 700 /var/lib/ogen/keys

# Statically-linked Go binary (CGO_ENABLED=0 in the build stage).
COPY --from=go-builder /server /server
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
COPY --from=geoip /geoip/dbip-city-lite.mmdb /usr/share/geoip/dbip-city-lite.mmdb
ENV GEOIP_DB_PATH=/usr/share/geoip/dbip-city-lite.mmdb

# The container starts as root so the entrypoint can chown the mounted KEK
# volume (Railway/Docker mount it as root, shadowing the build-time chown); the
# entrypoint then drops to the unprivileged appuser via su-exec. No `USER` here.
ENV ADDR=":3000"

EXPOSE 3000

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
