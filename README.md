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
   - Reads the original file through an `os.Root` anchored at `storage.base_dir`.
   - Builds `bimg.Options` for the requested format; JPEG inputs are flattened with a white background to avoid transparent padding.
   - Processes the image and writes only the requested format/geometry to the cache.
6. **Response** – sends the cached file with the appropriate `Content-Type`, `Cache-Control`, `ETag`, and `Last-Modified` headers.

No background conversions are performed—each request produces exactly one cached artefact matching the requested format.

`GET` and `HEAD` are both served. Client errors are reported as such rather than as
500s: a geometry above the limits, a geometry whose derived second side would exceed
the limits, a geometry that would shrink the source below one pixel on an axis, and a
path containing a NUL byte return `400`; a missing path, a path that is not a regular
file, and a path that leaves `storage.base_dir` through a symlink return `404`; an
unsupported extension, or bytes that libvips cannot decode (including a zero-byte
original), return `415`.

`0x0` — what the PrestaShop module emits when it knows no dimensions — is **not** an
error: it means "fit inside `resize.max_width` x `resize.max_height`, keeping the
aspect ratio, never upscaling". A single side of `0` (`200x0`, `0x200`) still means
"derive the other side from the source aspect ratio", and that derived side is subject
to the same limits as an explicit one.

Originals are opened through an `os.Root` anchored at `storage.base_dir`, so a symlink
inside it — file or directory — cannot serve, or cache, anything from outside it.

A resize is admitted through a semaphore sized from `runtime.resize_concurrency`
(default: one slot per `GOMAXPROCS`), released as soon as the resize returns rather
than when the response is finished. A request whose client has already disconnected is
dropped before the work starts; one that disconnects while the resize runs still gets
its result written to the cache, so the next visitor does not pay for it again.


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

## Monitoring

FARS exposes Prometheus metrics at `/metrics` on the API listener. Set
`metrics.listen` to move the endpoint to its own `host:port` — the deployment
where the resize port is proxied to the internet and only the metrics port is
published on a private (e.g. WireGuard) interface. `metrics.enabled: false`
unregisters the route entirely.

| Metric | Type | What it answers |
|---|---|---|
| `fars_http_requests_total{route,method,code,cache}` | counter | request rate, error ratio, and the cache hit ratio (`cache="hit"` vs `"miss"`) |
| `fars_http_request_duration_seconds{route,cache}` | histogram | latency, separable into hits and misses — averaging them together hides the resize cost |
| `fars_http_response_bytes_total{route,format}` | counter | bytes served per output format |
| `fars_request_errors_total{reason}` | counter | why requests failed: `bad_request`, `not_found`, `unsupported_media`, `unauthorized`, `internal` |
| `fars_resize_duration_seconds{format}` | histogram | time inside libvips alone, per output format |
| `fars_resize_queue_wait_seconds` | histogram | time spent waiting for an admission slot |
| `fars_resize_in_flight` / `fars_resize_slots` | gauge | saturation: the ratio of the two is how full the resize pipeline is |
| `fars_resize_source_bytes_total` | counter | original bytes read for resizing |
| `fars_cache_writes_total{result}` | counter | variants published, and failures to publish |
| `fars_cache_removals_total{reason}`, `fars_cache_removed_bytes_total{reason}` | counter | what the cache deletes and why: `ttl`, `orphan`, `outdated`, `size`, `invalidation`, `stale_temp` |
| `fars_cache_size_bytes`, `fars_cache_files`, `fars_cache_size_limit_bytes` | gauge | cache footprint as of the last sweep, against the configured cap |
| `fars_cache_sweep_duration_seconds`, `fars_cache_sweep_last_success_timestamp_seconds` | histogram, gauge | whether cleanup still finishes inside its interval |
| `fars_originals_index_ready` | gauge | 1 once discovery completed. **While it is 0 a changed original is not noticed** — the one to alert on |
| `fars_originals_tracked` | gauge | originals the monitor watches |
| `fars_originals_changed_total`, `fars_originals_check_errors_total` | counter | sources seen to change; checks that failed and were deferred |
| `fars_invalidations_total{result}` | counter | manual invalidation calls |
| `fars_gomaxprocs`, `fars_vips_concurrency`, `fars_cpu_quota` | gauge | the CPU settings in force and the cgroup limit they are measured against — `fars_gomaxprocs > fars_cpu_quota` is the throttling condition, and it alerts instead of being checked by hand |
| `fars_build_info{version,go_version}` | gauge | always 1, for joining a dashboard to a build |

Plus the standard `go_*` and `process_*` collectors. The request path is never
a label — it is attacker-controlled, and one crawler would mint a series per
URL. Anything gin cannot route is counted once as `route="unknown"`.

Series with a bounded label set (error reasons, removal reasons, formats) are
created at startup so they read `0` from the first scrape, rather than
appearing only once something goes wrong.

### Logging

`logging.level` (`debug`, `info`, `warn`, `error`) and `logging.format` (`text`
or `json`) control the log handler. `text` is the default, because shippers in
front of FARS parse the slog `level=INFO` form — switch a deployment's pipeline
and this setting together, or the lines arrive unlabelled.

`logging.access: false` drops the per-request `served image` line. Request
rate, latency and cache ratio are all in the metrics above; the line is only
worth its volume when the individual paths matter.


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
  # default: loopback + the private ranges; [] believes no proxy at all
  trusted_proxies: ["127.0.0.1/32", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]

storage:
  base_dir: "/var/www/prestashop/img"
  cache_dir: "/var/cache/img-resize"

resize:
  max_width: 2000
  max_height: 2000
  jpg_quality: 77
  webp_quality: 65
  avif_quality: 50
  avif_speed: 6
  png_compression: 6

cache:
  ttl: "10d"
  cleanup_interval: "12h"
  check_originals_interval: "5m"
  check_originals_workers: 4
  invalidation_lock_timeout: "100ms"
  invalidation_token: "" # empty disables both manual invalidation routes
  max_size: "50gb" # 0 disables size eviction; the cache is then bounded only by ttl

runtime:      # all three derive themselves; see CPU settings below
  gomaxprocs: 0
  vips_concurrency: 0
  resize_concurrency: 0

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
- `cache.ttl`, `cache.cleanup_interval`, and `cache.check_originals_interval` accept human-friendly duration strings (`30d`, `12h30m`, `45s`) or a bare unquoted number, interpreted as seconds (e.g. `ttl: 3600`). Use `0` (or `"0"`) to disable the corresponding background job.
- `cache.check_originals_workers` sets the size of each check/deletion pool (default 4; 0 selects the default). Discovery also uses a bounded pool of that size. `cache.invalidation_lock_timeout` bounds a background invalidation attempt between filesystem calls (default 100ms; 0 selects the default). A blocking OS syscall itself cannot be interrupted by this timeout.
- `cache.invalidation_token` enables the manual POST routes; keep it empty unless the endpoint is protected and needed.
- `cache.max_size` caps the total size of `cache_dir`. Eviction runs after the TTL pass and removes least-recently-used entries until the cache is back under the cap. Empty or `0` means no cap, and the cache is then bounded only by `ttl` — since every distinct geometry writes a new file, an unauthenticated client can fill the volume. Set it in production.
- Setting a cap also changes what `ttl` measures. With no cap, a variant's mtime is its publication time, so `ttl` expires it that long after it was generated. With a cap, a cache hit refreshes the variant's mtime (at most once an hour, so it costs nothing on a hot path), which makes both `ttl` and size eviction act on time since last use — the recency an image cache actually wants, and the reason eviction doesn't throw away the catalogue images every page loads.
- `storage.base_dir` must already exist: FARS refuses to start otherwise instead of creating it. A missing bind mount used to produce an empty directory, a service that looked healthy, and a cleanup pass that deleted the whole variant cache as "orphans". `cache_dir` is still created on demand, and the two directories may not overlap (neither may be `/`, contain the other, or be the same path).
- `cache.max_size` defaults to `50gb`. The alternative default is "grow until the volume is full", since every distinct geometry writes another file and only `ttl` removes them. Raise it on a bigger disk; `0` restores unbounded growth deliberately.
- `runtime.resize_concurrency` caps how many resizes run at once; `0` means one per `GOMAXPROCS`. Keep it separate from `vips_concurrency`: that one sizes the thread pool *inside* a single libvips operation, which is not a number of concurrent requests.
- `metrics.enabled`, `metrics.path` and `metrics.listen` configure the Prometheus endpoint — see [Monitoring](#monitoring). `metrics.path` is rejected at startup if it collides with the `/resize`, `/cache` or `/cclear` routes, since gin would panic on the overlap instead of reporting it.
- `logging.level`, `logging.format` and `logging.access` configure log output.
- `server.trusted_proxies` lists the reverse proxies (IPs or CIDRs) whose `X-Forwarded-For` may be believed, which is what the access log's `remote_ip` reports. Empty (the default) trusts none, so `remote_ip` is the peer that opened the connection — with Angie in front, that is Angie. Set it to the proxy's address to see real client IPs; never widen it to a range that reaches untrusted clients, or they can forge `remote_ip`.
- Rewrite rules are evaluated sequentially; the first matching pattern rewrites the path and stops the chain.

### CPU settings

Leave `runtime.gomaxprocs` and `runtime.vips_concurrency` at `0`. Both derive
themselves at startup, and both exist because the two libraries underneath
count CPUs differently:

- **Go** has read the cgroup CPU quota since 1.25, so a container limited to
  `cpus: 6` on a 64-core host gets `GOMAXPROCS=6` without being told, and keeps
  tracking the limit if it changes. Setting the value by hand replaces that and
  freezes it, so the number has to be kept in step with the compose file by
  somebody remembering to. A value above the detected limit is logged as a
  warning at startup — more schedulable threads than the quota can run means
  the kernel stops the process part of every scheduling period.
- **libvips** counts the host's CPUs and ignores the quota entirely, so left
  alone it starts a thread pool sized for the machine inside a container that
  may only run a fraction of it. FARS therefore always sets it explicitly;
  `0` means one thread per operation, which puts the parallelism across
  requests (`resize_concurrency`) rather than inside a single resize.

Both effective values are published as `fars_gomaxprocs` and
`fars_vips_concurrency`, and logged at startup, so the check is a query rather
than an `exec` into the container.

### Environment Overrides

Every option in the YAML can be supplied through environment variables. Two naming styles are supported, and **`FARS_` is the preferred one** — it always wins when both are set for the same setting:

- **Scoped (`FARS_`, preferred)** – prefix with `FARS_` and join nested keys with double underscores. Any config key can be reached this way. Examples:
  - `FARS_SERVER__PORT=8080`
  - `FARS_SERVER__HOST=127.0.0.1`
  - `FARS_STORAGE__BASE_DIR=/srv/images`
- **Legacy shortcuts (unprefixed)** – a fixed, explicit allowlist kept only for backward compatibility with the shipped `Dockerfile` and existing deployments: `PORT`, `IMAGES_BASE_DIR`, `CACHE_DIR`, `TTL`, `CLEANUP_INTERVAL`, `CHECK_ORIGINALS_INTERVAL`, `INVALIDATION_TOKEN`, `MAX_CACHE_SIZE`, plus the resize quality/limit keys (`MAX_WIDTH`, `MAX_HEIGHT`, `JPG_QUALITY`, `WEBP_QUALITY`, `AVIF_QUALITY`, `PNG_COMPRESSION`, `AVIF_SPEED`, `GOMAXPROCS`, `VIPS_CONCURRENCY`, `RESIZE_CONCURRENCY`). This surface is intentionally narrow: it does not support the `FOO__BAR` nested-path syntax, and it does **not** include a generic `HOST` — an ambient, unrelated `HOST` variable in the environment can no longer repoint the service. Use `FARS_HOST` for that. New deployments should prefer `FARS_`-prefixed variables throughout. The
metrics and logging settings are deliberately not in the legacy list:
`FARS_METRICS__ENABLED`, `FARS_METRICS__LISTEN`, `FARS_LOGGING__FORMAT` and so
on are the only spellings.

Environment values override the built-in defaults and anything read from YAML. Between the two styles, the `FARS_`-prefixed value is applied last and wins if a setting is provided both ways. Duration strings support the same syntax as the config file (`36h`, `15m30s`), and byte sizes accept units like `512kb`, `2mb`, `1giB`.

## Development Notes

- Run `make test` for the full gate: race detector on, `-tags=integration` included. `make test-short` is the fast path without either. CI runs the same gate plus `gofmt`, `go vet`, staticcheck and govulncheck, and the image is only published if it passes.
- Run the real filesystem inventory with `FARS_SCAN_BASE_DIR=/path/to/site FARS_SCAN_ORIGINALS_DIR=/path/to/site/img FARS_SCAN_CACHE_DIR=/path/to/cache go test -tags=integration ./internal/cache -run TestScanOriginalsAndResizes -v`.
- If running tests in a sandboxed environment, set a local build cache: `GOCACHE=$(pwd)/.gocache go test ./...`.
- Make sure `libvips` is reachable through your dynamic linker, otherwise `bimg` will fail at runtime.

## Roadmap

- Add end-to-end tests against the Gin router with a temporary filesystem.
- Expose configurable logging levels and basic metrics.


## Docker

Published as [`dementev/fars`](https://hub.docker.com/r/dementev/fars) for `linux/amd64` and `linux/arm64`. The page shown there is [`docs/dockerhub.md`](docs/dockerhub.md), which documents the image itself — volumes, the uid it runs as, and every environment variable it honours.

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
