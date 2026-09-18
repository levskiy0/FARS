# FARS — Fast Auto Resize Service

On-demand image resizing over HTTP. FARS takes a directory of original images,
resizes them as they are requested, and keeps every generated variant in a disk
cache that invalidates itself when the original changes.

It was written for PrestaShop image trees (`img/p/1/2/12.jpg`, category images,
the usual Nginx rewrites) but nothing in it is PrestaShop-specific: point it at
any directory of static images.

- Source, full documentation, issues: <https://github.com/levskiy0/FARS>
- Image: `dementev/fars:latest` — `linux/amd64`, `linux/arm64`

## What it does

- One public route: `GET|HEAD /resize/{width}x{height}/{path}`.
- Encodes JPEG, PNG, WebP and AVIF through libvips (AVIF in and out, via the
  libheif AV1 plugins that ship in the image).
- Picks the output format from a "double extension": `12.jpg.webp` resizes
  `12.jpg` and returns WebP. The base file is found transparently.
- Caches to `cache_dir/{geometry}/{path}`. No hash, version or timestamp is
  added to the URL, so the cache directory can be served directly by Nginx or
  Angie with `try_files`, and FARS only sees the misses.
- Watches the originals referenced by the cache in the background and drops
  every cached geometry and format of a source that changed.
- Purges cache entries by TTL, and — with `cache.max_size` set — by least
  recent use once the cache exceeds the cap.
- Exports Prometheus metrics on `/metrics`: request rate and latency split by
  cache hit and miss, resize time per format, admission-queue wait, cache size
  against its cap, removals by reason, and whether the originals index is
  ready.

## Quick start

```bash
docker run --rm -p 9090:9090 \
  -v /var/www/prestashop/img:/app/data/images:ro \
  -v fars-cache:/app/data/cache \
  dementev/fars:latest
```

```bash
curl -o thumb.jpg 'http://127.0.0.1:9090/resize/300x300/p/1/2/12.jpg'
curl -o thumb.webp 'http://127.0.0.1:9090/resize/300x300/p/1/2/12.jpg.webp'
```

With compose:

```yaml
services:
  fars:
    image: dementev/fars:latest
    restart: unless-stopped
    ports: ["9090:9090"]
    environment:
      FARS_RESIZE__MAX_WIDTH: 2000
      FARS_RESIZE__MAX_HEIGHT: 2000
      FARS_CACHE__MAX_SIZE: 50gb
      FARS_CACHE__TTL: 30d
      FARS_SERVER__TRUSTED_PROXIES: 172.16.0.0/12
    volumes:
      - /var/www/prestashop/img:/app/data/images:ro
      - fars-cache:/app/data/cache

volumes:
  fars-cache:
```

The entrypoint is `/app/fars serve`, so `command:` is only needed to add flags —
`--config /app/config/config.yaml` to load a YAML file instead of configuring
through the environment.

## Image layout

| | |
|---|---|
| Base | `debian:trixie-slim` + libvips, libheif AV1 plugins, ca-certificates, tzdata |
| User | `fars`, uid/gid 10001 — the cache volume must be writable by it |
| Port | 9090 |
| Originals | `/app/data/images` (must exist; mount it read-only) |
| Cache | `/app/data/cache` (must be writable; use a named volume) |
| Entrypoint | `/app/fars serve` |

## Configuration

Everything can be set through the environment; a YAML file is optional. Prefix a
config key with `FARS_` and join nested keys with a double underscore:
`FARS_SERVER__PORT`, `FARS_STORAGE__BASE_DIR`, `FARS_RESIZE__JPG_QUALITY`,
`FARS_CACHE__INVALIDATION_TOKEN`. That form reaches every setting and always
wins over the legacy unprefixed shortcuts below.

Defaults baked into the image:

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `9090` | listen port |
| `IMAGES_BASE_DIR` | `/app/data/images` | originals root; must already exist |
| `CACHE_DIR` | `/app/data/cache` | cache root; created on demand |
| `TZ` | `Etc/UTC` | |

The image sets nothing else. Environment beats YAML in the loader, so a tuning
value baked into the image would silently overrule a mounted config file —
which is what an earlier `TTL=24h` in this image did to deployments that had
set `ttl: 10d` in their config. Retention and encoder settings come from your
config file, or from the defaults below.

Other common settings (legacy shortcut on the left, all also reachable as
`FARS_…`):

| Variable | Default | Meaning |
|---|---|---|
| `MAX_WIDTH`, `MAX_HEIGHT` | `2000` | geometry ceiling; beyond it the request is a `400` |
| `TTL` | `10d` | how long a cached variant survives |
| `CLEANUP_INTERVAL` | `12h` | how often the TTL pass runs |
| `JPG_QUALITY`, `WEBP_QUALITY`, `AVIF_QUALITY` | `77`, `65`, `50` | encoder quality, measured on product photography |
| `AVIF_SPEED` | `6` | libheif AVIF effort, 0 slowest/best … 8 fastest |
| `PNG_COMPRESSION` | `6` | zlib level |
| `MAX_CACHE_SIZE` | `50gb` | total cache cap. `0` disables size eviction, and the cache is then bounded only by TTL while every distinct geometry writes another file. Raise it on a bigger disk |
| `CHECK_ORIGINALS_INTERVAL` | `5m` | how often tracked originals are re-checked |
| `INVALIDATION_TOKEN` | empty | bearer token; empty disables both invalidation routes |
| `RESIZE_CONCURRENCY` | `0` | concurrent resizes; `0` = one per `GOMAXPROCS` |
| `VIPS_CONCURRENCY` | `0` | threads *inside* one libvips operation; `0` means 1. Not a request-concurrency knob |
| `GOMAXPROCS` | `0` | `0` lets the Go runtime read the container's CPU quota — see below |
| `FARS_SERVER__TRUSTED_PROXIES` | empty | proxies (IPs or CIDRs) whose `X-Forwarded-For` is believed. Empty logs the connecting peer, i.e. your reverse proxy |
| `FARS_METRICS__ENABLED` | `true` | serve Prometheus metrics |
| `FARS_METRICS__PATH` | `/metrics` | where they are served |
| `FARS_METRICS__LISTEN` | empty | move metrics to their own `host:port`, so only that port need be reachable from the monitoring network. Publish it separately: `-p 10.0.0.1:9091:9091` |
| `FARS_LOGGING__LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `FARS_LOGGING__FORMAT` | `text` | `text` or `json`. Log shippers that parse the slog `level=INFO` form need `text`; change both together |
| `FARS_LOGGING__ACCESS` | `true` | the per-request line. Rate, latency and cache ratio are in the metrics either way |

Rewrite rules (regex → replacement, first match wins) let a public URL differ
from the path on disk; they are configured in YAML only. The sample config in
the repository ships the PrestaShop set, which maps `12-large_default/name.jpg`
to `img/p/1/2/12.jpg`.

## CPU

Leave `GOMAXPROCS` and `VIPS_CONCURRENCY` unset. Go reads the container's CPU
quota itself, so `--cpus=6` gives `GOMAXPROCS=6` on a 64-core host without
being told, and keeps tracking the limit if it changes; pinning the value by
hand replaces that and has to be kept in step with the limit by somebody
remembering to. libvips, by contrast, counts the *host's* CPUs and ignores the
quota, so FARS always sets it explicitly — one thread per operation, with the
parallelism across requests instead.

A `GOMAXPROCS` above the container's quota is logged as a warning at startup —
more runnable threads than the quota can serve means the kernel stops the
process for the rest of every scheduling period. FARS reads the quota from the
cgroup rather than from the runtime, because a `GOMAXPROCS` variable overrides
it inside the runtime. The effective values are published as `fars_gomaxprocs`,
`fars_vips_concurrency` and `fars_cpu_quota`, so `fars_gomaxprocs >
fars_cpu_quota` can alert.

## Geometry

`{width}x{height}` with either side `0` (or omitted: `200x`, `x200`) means
"derive that side from the source aspect ratio". `0x0` means "fit inside
`MAX_WIDTH` × `MAX_HEIGHT`, keeping the aspect ratio, never upscaling". A
derived side is capped exactly like an explicit one, so a tall source cannot
turn a small width into a huge height.

## Status codes

| Code | When |
|---|---|
| `200` | resized (or served from cache) |
| `304` | `If-None-Match` / `If-Modified-Since` matched |
| `400` | geometry over the limits, a derived side over the limits, a geometry that would shrink the source below one pixel, or a NUL byte in the path |
| `404` | no such original, not a regular file, or a path that leaves the originals root through a symlink |
| `415` | extension FARS does not encode, or bytes libvips cannot decode (including a zero-byte original) |

## Monitoring

`GET /metrics` returns the Prometheus exposition, including the `go_*` and
`process_*` collectors. The families are documented in the
[README](https://github.com/levskiy0/FARS#monitoring); the two worth an alert
are `fars_originals_index_ready` (0 means a changed original will not be
noticed) and `fars_cache_sweep_last_success_timestamp_seconds` (cleanup no
longer finishing inside its interval).

The request path is never used as a label, so a crawler cannot inflate
cardinality.

## Manual invalidation

With `INVALIDATION_TOKEN` set, two POST routes are registered (with it empty,
neither exists). Paths are relative to the originals root, with no geometry and
no output suffix; every cached geometry and format of that original is removed,
the original itself never is.

```bash
curl -X POST http://127.0.0.1:9090/cclear/p/1/2/12.jpg \
  -H "Authorization: Bearer $INVALIDATION_TOKEN"

curl -X POST http://127.0.0.1:9090/cache/invalidate \
  -H "Authorization: Bearer $INVALIDATION_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"paths":["p/1/2/12.jpg","c/42.jpg"]}'
```

Batch requests are limited to 64 KiB and 1000 paths.

## Notes for production

- Mount the originals read-only. FARS never writes to them, and the cache is the
  only volume that needs to be writable.
- Check `MAX_CACHE_SIZE` against the volume you gave the cache. With a cap in
  place a cache hit refreshes the entry's mtime (at most hourly), so both
  eviction and TTL measure time since last use.
- The originals root must exist at startup — FARS refuses to start rather than
  create it, because an unmounted volume would otherwise look healthy and the
  cleanup pass would delete the whole cache as orphans. The two directories may
  not overlap.
- Behind a reverse proxy, serve the cache directory directly (`try_files` onto
  the cache volume) and let FARS handle only the misses.
