# Friendly Assets Resize Server (fars)

`fars` is an HTTP image resizing service written in Go. It takes originals from a filesystem tree, resizes them on demand, and keeps the generated variants in a cache directory. The project originated for PrestaShop installations but works with any directory of static images.

## Highlights

- Single endpoint: `/resize/{width}x{height}/{path}` (e.g. `/resize/200x200/img/p/1/13.jpg`).
- Outputs JPEG, PNG, WebP, or AVIF using libvips through [`bimg`](https://github.com/h2non/bimg).
- Understands "double extensions" (`13.jpg.webp`, `item.png.avif`, etc.) and falls back to the base file transparently.
- When the source file is JPEG/JPG the result is flattened onto a white background so resized variants never end up semi-transparent.
- Disk cache organised as `cache_dir/{width}x{height}/…`; public resize URLs and cache paths never contain a version or content hash.
- Background original monitor invalidates every cached geometry/format when a referenced source changes.
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


## Cache invalidation and original monitor

Подробное описание на русском: [кеш и фоновый monitor](docs/cache-monitor.md).

The public API remains `/resize/{width}x{height}/{path}` and the generated filename remains `cache_dir/{geometry}/{path}`. No hash, version, or timestamp is added to the URL.

At service startup FARS launches the monitor in its own goroutine. Bootstrap walks `cache_dir` first, groups resize files by relative path, and resolves only the originals referenced by those files. It never walks the complete `storage.base_dir`, so a 70 GB original tree does not determine the cost; the number of existing cached resize files does. Double-extension entries such as `photo.jpg.webp` are mapped back to `photo.jpg` when an exact original does not exist. Orphan cache files are removed.

For every referenced original the index keeps its relative path, metadata signature, and exact cached variant paths. The signature contains file size and nanosecond `mtime`; on Linux and macOS it also includes inode and `ctime`. The monitor does not open source contents or calculate source hashes. The existing response ETag calculation is unchanged. Newly generated variants are registered immediately, and TTL cleanup unregisters files it removes.

A compact snapshot is stored at `cache_dir/.fars-originals-v1.json`. It is read once during bootstrap and rewritten atomically by an independent persistence loop when the tracked state changes, and on clean shutdown. During incomplete discovery the snapshot preserves previous baselines for unresolved paths. It is not read for HTTP requests. For entries included in the snapshot, it lets a new FARS process detect source replacement that happened while it was stopped, including replacement with the same size and preserved `mtime`. The same limitation applies to missing/corrupt snapshots and new entries not flushed before an abrupt shutdown. On the first deployment, when no snapshot exists yet, startup can only identify pre-existing stale entries whose source `mtime` is newer than the resize; subsequent changes use the full signature.

Every `cache.check_originals_interval` a fixed worker pool checks the in-memory source list. An original cannot have two checks in flight; other originals can be checked on later ticks even if one syscall remains blocked. Changes are queued for an independent deletion pool, retried at most one second later when capacity is available. Lock waiting has a configurable timeout. Bootstrap errors retain failed directories/references for retry while known originals remain monitored; manifest write errors never gate checking or deletion. With Nginx `try_files` against the same cache volume, a request after file deletion falls through to FARS, which recreates only the requested resize. If `open_file_cache` is enabled, its validation interval may delay this. An outer `proxy_cache` is a separate HTTP cache and requires its own expiry or purge.

This invalidates the shared disk cache used by Nginx. A browser or upstream CDN that already accepted a one-year `immutable` response may continue displaying its private copy without contacting Nginx. If same-URL changes must become visible to clients within X minutes, remove `immutable` and set the browser/CDN `max-age` to no more than X, or purge that outer cache separately. Server-side file deletion cannot revoke a response already stored in a client.

### Manual invalidation

Set `cache.invalidation_token` to enable `POST /cclear/{path}` and the existing batch `POST /cache/invalidate`. With an empty token neither route is registered. Supply original paths relative to `storage.base_dir`, without a geometry or output suffix. Configured rewrites apply. All paths are validated before deletion; batch requests are limited to 64 KiB and 1000 paths.

For one original, send a bodyless POST (URL-encode spaces and other reserved characters once):

```bash
curl -X POST http://127.0.0.1:9090/cclear/img/p/1/2/12.jpg \
  -H "Authorization: Bearer $FARS_INVALIDATION_TOKEN"
```

The response is `{"invalidated":["img/p/1/2/12.jpg"],"variants_removed":3}`.
All cached geometries and output formats of that original are removed. The original is never deleted.
Repeating the request succeeds with zero removals when no variants remain; a deleted original can also be cleared.
Pass the actual original-relative path (or a configured rewrite alias), not a resize URL.
The path is decoded once; a literal percent sign must be encoded as `%25`.

For a batch:

```bash
curl -X POST http://127.0.0.1:9090/cache/invalidate \
  -H "Authorization: Bearer $FARS_INVALIDATION_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"paths\":[\"img/p/1/2/12.jpg\",\"img/cb/42.jpg\"]}"
```

The endpoint removes all currently cached sizes and output formats for each supplied original. If bootstrap has not finished or an original is not in memory, the endpoint performs a one-off cache scan for that path.

### Validation and review

The [detailed cache documentation](docs/cache-monitor.md) includes token setup, POST response codes, review findings, remaining reliability limits, and reproducible test commands. Validation passed with `go test -race ./...` and `go vet ./...`; selected regression tests passed 10 repetitions and the real PNG/WebP invalidation/regeneration lifecycle passed 5 repetitions. The review fixed partial batch deletion on invalid paths and stale orphan tasks deleting regenerated resizes.

These tests use temporary data, not production images. They verify FARS's disk cache, not an external Nginx/CDN HTTP cache. Existing resize URLs and cache paths remain unchanged.

## Requirements

- Go 1.27+
- libvips installed on the host (required by `bimg`).

## Quick Start

```bash
VERSION=$(git describe --tags --dirty --always 2>/dev/null || echo dev)
go build -ldflags "-X fars/internal/version.Version=${VERSION}" -o fars ./cmd/fars-server
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
  avif_speed: 6
  png_compression: 6

cache:
  ttl: "30d"
  cleanup_interval: "24h"
  check_originals_interval: "5m"
  check_originals_workers: 4
  invalidation_lock_timeout: "100ms"
  invalidation_token: "" # empty disables both manual invalidation routes

runtime:
  gomaxprocs: 0
  vips_concurrency: 0

rewrites:
  - pattern: "^(\\d)(-[\\w-]+)?/.+\\.jpg$"
    replacement: "img/p/$1/$1$2.jpg"
  - pattern: "^c/([\\w.-]+)/.+\\.jpg$"
    replacement: "img/c/$1.jpg"
```

Key points:

- `max_width` / `max_height` guard against excessive geometry. Requests beyond the limits return `400 Bad Request`.
- `jpg_quality`, `webp_quality`, `avif_quality`, and `png_compression` feed directly into the libvips encoder settings.
- `avif_speed` passes through to the libheif AVIF encoder (0 = slowest/best, 8 = fastest).
- `cache.ttl`, `cache.cleanup_interval`, and `cache.check_originals_interval` accept human-friendly durations (`30d`, `12h30m`, `45s`). Use `"0"` to disable the corresponding background job.
- `cache.check_originals_workers` sets the size of each check/deletion pool (default 4; 0 selects the default). Discovery also uses a bounded pool of that size. `cache.invalidation_lock_timeout` bounds a background invalidation attempt between filesystem calls (default 100ms; 0 selects the default). A blocking OS syscall itself cannot be interrupted by this timeout.
- `cache.invalidation_token` enables the manual POST routes; keep it empty unless the endpoint is protected and needed.
- `runtime.gomaxprocs` and `runtime.vips_concurrency` allow tuning Go scheduler threads and libvips worker pool (0 keeps library defaults).
- Rewrite rules are evaluated sequentially; the first matching pattern rewrites the path and stops the chain.

### Environment Overrides

Every option in the YAML can be supplied through environment variables. Two naming styles are supported:

- **Scoped** – prefix with `FARS_` and join nested keys with double underscores. Examples:
  - `FARS_SERVER__PORT=8080`
  - `FARS_STORAGE__BASE_DIR=/srv/images`
- **Legacy shortcuts** (kept for existing deployments): `PORT`, `IMAGES_BASE_DIR`, `CACHE_DIR`, `TTL`, `CLEANUP_INTERVAL`, `CHECK_ORIGINALS_INTERVAL`, `INVALIDATION_TOKEN`, plus the resize quality/limit keys.

Environment values override both the built-in defaults and anything read from YAML. Duration strings support the same syntax as the config file (`36h`, `15m30s`), and byte sizes accept units like `512kb`, `2mb`, `1giB`.

## Development Notes

- Run `go test ./...` for the unit tests.
- Run the real filesystem inventory with `FARS_SCAN_BASE_DIR=/path/to/site FARS_SCAN_ORIGINALS_DIR=/path/to/site/img FARS_SCAN_CACHE_DIR=/path/to/cache go test -tags=integration ./internal/cache -run TestScanOriginalsAndResizes -v`.
- If running tests in a sandboxed environment, set a local build cache: `GOCACHE=$(pwd)/.gocache go test ./...`.
- Make sure `libvips` is reachable through your dynamic linker, otherwise `bimg` will fail at runtime.

## Roadmap

- Add end-to-end tests against the Gin router with a temporary filesystem.
- Expose configurable logging levels and basic metrics.


## Docker

To build:
```bash
docker build -t fars:latest .
```

To run:
```bash
docker run --rm -p 9090:9090 \
  -e PORT=9090 \
  -e IMAGES_BASE_DIR=/app/data/images \
  -e CACHE_DIR=/app/data/cache \
  -e TTL=24h \
  -e CLEANUP_INTERVAL=10m \
  -v "./data/images:/app/data/images" \
  -v "./data/cache:/app/data/cache" \
  -v "./example.config.yaml:/app/config/example.config.yaml" \
  fars:latest \
  cmd --config /app/config/example.config.yaml
```

  --entrypoint	"/app/fars serve --config /app/config/example.config.yaml" \
