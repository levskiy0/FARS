# syntax=docker/dockerfile:1.7
#
# FARS image — Debian/glibc, same base as the other Docker Hub images here.
#
# Why debian-slim rather than alpine:
#   - one base and one CVE-patch cadence across the images we publish;
#   - glibc everywhere: the cgo build links the same libvips the runtime
#     loads, so no musl-only libvips/libheif build paths to chase;
#   - Debian ships libheif's AV1 codecs as separate plugin packages, so
#     AVIF encode/decode is an explicit dependency instead of alpine's
#     all-or-nothing vips-heif bundle.
#
# Cost: ~90 MB runtime instead of ~40 MB. Acceptable for a service that
# already carries libvips.

FROM mirror.gcr.io/library/golang:1.27-trixie AS builder

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends libvips-dev \
 && rm -rf /var/lib/apt/lists/*

ENV CGO_ENABLED=1
WORKDIR /app

# Build caches are keyed per target platform — the multi-arch CI build runs
# amd64 and arm64 concurrently and they must not share object files.
ARG TARGETPLATFORM

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,id=gomod-${TARGETPLATFORM} \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod,id=gomod-${TARGETPLATFORM} \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-${TARGETPLATFORM} \
    go build -trimpath -ldflags="-s -w" -o /out/fars ./cmd/fars-server


# Runtime: libvips + the AV1 plugins libheif needs for AVIF in and out.
FROM mirror.gcr.io/library/debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates tzdata \
        libvips42t64 \
        libheif-plugin-aomenc libheif-plugin-dav1d \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system --gid 10001 fars \
 && useradd --system --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin fars \
 && mkdir -p /app/data/images /app/data/cache \
 && chown -R fars:fars /app

WORKDIR /app
COPY --from=builder --chown=fars:fars /out/fars /app/fars
USER fars

# Only what describes this image's own filesystem and port. Tuning values are
# deliberately absent: environment beats YAML in the loader, so a TTL baked in
# here would silently overrule the ttl in a mounted config file — which is
# exactly what happened with the previous 24h/10m defaults. Retention now comes
# from the config file, or from the built-in defaults when there is none.
ENV TZ=Etc/UTC \
    PORT=9090 \
    IMAGES_BASE_DIR=/app/data/images \
    CACHE_DIR=/app/data/cache

EXPOSE 9090
ENTRYPOINT ["/app/fars","serve"]
