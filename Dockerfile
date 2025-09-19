FROM golang:1.24-alpine AS builder
# RUN apk add --no-cache vips-dev
RUN apk add --no-cache build-base vips-dev pkgconfig
# ENV CGO_ENABLED=0 GO111MODULE=on
ENV CGO_ENABLED=1 GO111MODULE=on
WORKDIR /app

# Pre-cache deps (later)
COPY go.mod go.sum ./
RUN go mod download

# Copy all app files
COPY . .

# Build
RUN ls -1ha
RUN go build -trimpath -ldflags="-s -w" -o /out/fars ./cmd/fars-server


# Slim runtime with libvips
FROM alpine:3.22
RUN apk add --no-cache vips ca-certificates
WORKDIR /app
RUN adduser -s /sbin/nologin -D fars
USER fars
# Copy the binary
COPY --chown=fars:fars --from=builder /out/fars .

VOLUME ["/app/data/cache"]

ENV PORT=9090 \
    IMAGES_BASE_DIR=/app/data/images \
    CACHE_DIR=/app/data/cache \
    TTL=24h \
    CLEANUP_INTERVAL=10m

EXPOSE 9090
ENTRYPOINT ["/app/fars","serve"]