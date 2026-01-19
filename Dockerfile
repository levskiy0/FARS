FROM golang:1.24-alpine AS builder
RUN apk add --no-cache build-base vips-dev vips-heif pkgconfig
ENV CGO_ENABLED=1 \
    GO111MODULE=on
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

# Copy all app files
COPY . .

# Build
RUN ls -1ha
RUN go build -trimpath -ldflags="-s -w" -o /out/fars ./cmd/fars-server


# Slim runtime with libvips
FROM alpine:3.23
WORKDIR /app
RUN apk add --no-cache vips vips-heif && \
    rm -rf /var/cache/apk/*  && \
    adduser -H -D -u 10001 -s /sbin/nologin fars && \
    mkdir -p /app/data/images /app/data/cache && \
    chown -R fars:fars /app
USER fars
# Copy the binary
COPY --chown=fars:fars --from=builder /out/fars .

ENV PORT=9090 \
    IMAGES_BASE_DIR=/app/data/images \
    CACHE_DIR=/app/data/cache \
    TTL=24h \
    CLEANUP_INTERVAL=10m

EXPOSE 9090
ENTRYPOINT ["/app/fars","serve"]
