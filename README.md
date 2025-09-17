# Friendly Assets Resize Server (fars)

`fars` is an HTTP image resizing service written in Go. It takes originals from a filesystem tree, resizes them on demand, and keeps the generated variants in a cache directory. The project originated for PrestaShop installations but works with any directory of static images.

## Highlights

- Single endpoint: `/resize/{width}x{height}/{path}` (e.g. `/resize/200x200/img/p/1/13.jpg`).
- Outputs JPEG, PNG, WebP, or AVIF using libvips through [`bimg`](https://github.com/h2non/bimg).
- Understands "double extensions" (`13.jpg.webp`, `item.png.avif`, etc.) and falls back to the base file transparently.
- When the source file is JPEG/JPG the result is flattened onto a white background so resized variants never end up semi-transparent.
- Disk cache organised as `cache_dir/{width}x{height}/…` with freshness checks based on modification time and an optional TTL.
- Configurable cleanup job that purges stale cache entries.
- Regex rewrite rules to mimic typical Nginx rewrites from PrestaShop land.

## How a Request Is Served

1. **Geometry parsing** – handles fixed dimensions (e.g. `200x200`), allows zero for a free side (`0x400` ⇒ height 400, width auto), and accepts shorthand like `120x` / `x120` which map to the same behaviour.
2. **Path normalisation** – strips the leading slash, converts path separators to `/`, and executes the configured rewrite rules until the first match.
3. **Source lookup** –
   - Checks the exact path requested.
   - If missing and the path ended with a double extension, trims the last extension and tries the base (`13.jpg.webp` → `13.jpg`).
   - Returns `404 Not Found` when no candidate exists.
4. **Cache probe** – looks for `cache_dir/{geometry}/{path}` (double extensions append to the base path). A fresh entry is served immediately.
5. **Resize** –
   - Reads the original file (`os.ReadFile`).
   - Builds `bimg.Options` for the requested format; JPEG inputs are flattened with a white background to avoid transparent padding.
   - Processes the image and writes only the requested format/geometry to the cache.
6. **Response** – sends the cached file with the appropriate `Content-Type`, `Cache-Control`, `ETag`, and `Last-Modified` headers.

No background conversions are performed—each request produces exactly one cached artefact matching the requested format.

## Requirements

- Go 1.21+
- libvips installed on the host (required by `bimg`).

## Quick Start

```bash
go build -o fars ./cmd/fars-server
./fars serve --config ./config.yaml

# or without building ahead of time

go run ./cmd/fars-server/main.go serve --config ./config.yaml
```

Smoke test:

```bash
curl -o thumb.jpg \
  "http://127.0.0.1:9090/resize/300x300/test/13.jpg"
```

After the first request you will find `cache_dir/300x300/test/13.jpg`. A request such as `…/13.jpg.webp` writes `cache_dir/300x300/test/13.jpg.webp`—and nothing else.

## Configuration

Sample `config.yaml`:

```yaml
server:
  host: 0.0.0.0
  port: 9090

storage:
  base_dir: "/var/www/prestashop/img"
  cache_dir: "/var/cache/img-resize"

resize:
  max_width: 2000
  max_height: 2000
  jpg_quality: 80
  webp_quality: 75
  avif_quality: 45
  png_compression: 6

cache:
  ttl: "30d"
  cleanup_interval: "24h"

rewrites:
  - pattern: "^(\\d)(-[\\w-]+)?/.+\\.jpg$"
    replacement: "img/p/$1/$1$2.jpg"
  - pattern: "^c/([\\w.-]+)/.+\\.jpg$"
    replacement: "img/c/$1.jpg"
```

Key points:

- `max_width` / `max_height` guard against excessive geometry. Requests beyond the limits return `400 Bad Request`.
- `jpg_quality`, `webp_quality`, `avif_quality`, and `png_compression` feed directly into the libvips encoder settings.
- `cache.ttl` and `cache.cleanup_interval` accept human-friendly durations (`30d`, `12h30m`, `45s`).
- Rewrite rules are evaluated sequentially; the first matching pattern rewrites the path and stops the chain.

## Development Notes

- Run `go test ./...` for the lightweight unit tests (configuration loader + HTTP handler helpers).
- If running tests in a sandboxed environment, set a local build cache: `GOCACHE=$(pwd)/.gocache go test ./...`.
- Make sure `libvips` is reachable through your dynamic linker, otherwise `bimg` will fail at runtime.

## Roadmap

- Add end-to-end tests against the Gin router with a temporary filesystem.
- Expose configurable logging levels and basic metrics.
