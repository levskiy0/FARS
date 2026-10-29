# FARS — Fast Auto Resize Service

> Part of the **Citimarine umbrella workspace**. See `../CLAUDE.md` for the full topology and rules.

## What this repo is

Go source for FARS, the image-resize service Citimarine uses to generate thumbnail/responsive variants on demand. Built into the `dementev/fars:latest` Docker image, which runs as the `fars-1` service in the front stack (container `<env>-ctm-fars`, `local-ctm-fars` locally) on every environment.

- Remote: `git@github.com:levskiy0/FARS.git` (**GitHub**, not GitLab — the only repo in the umbrella hosted on GitHub)
- Default branch: `main`; day-to-day work on `develop`

## Layout

`cmd/fars-server` (entrypoint) · `internal/` (implementation) · `pkg/` (exported packages) ·
`tests/` (fixture images + cache) · `data/{images,cache}` (local run scratch, gitignored) · `docs/` · `example.config.yaml` (copy to `config.yaml` to run; gitignored). Packages under `internal/`: `app`, `cache`, `config`, `httpapi`, `locker`, `metrics`, `processor`, `server`, `version`.

CI is GitHub Actions (`.github/workflows/`): `test.yml` on `develop` pushes and PRs (gofmt, vet, staticcheck, race + integration tests, govulncheck; libvips installed); `docker-build-push.yml` on `main` pushes builds linux/amd64 + arm64 and pushes `dementev/fars` to Docker Hub, then syncs `docs/dockerhub.md` as the Hub description. Every env's compose references `dementev/fars:latest`, so a `main` push publishes what the next pull deploys.

## Commands

Go 1.27 (`go.mod`). Use the `Makefile`, not raw `go` invocations:

```bash
make build       # → ./fars binary, version stamped from `git describe`
make run         # build + serve --config ./config.yaml (override: CONFIG=…)
make test        # go test -race -tags=integration ./...  — what CI runs; without the tag the end-to-end resize tests skip silently
make test-short  # go test ./...  (no race detector, no integration tag)
make lint        # go vet + staticcheck (fetched on demand, needs network)
make fmt         # gofmt -w ./cmd ./internal ./pkg ./tests   (fmt-check: fail instead of write)
make clean       # remove the binary
```

`GOCACHE` is pinned to `./.gocache`. `make test-race` is an alias of `make test`. The repo-root `fars` binary is a build artifact (gitignored).

## Local dev integration

FARS's Go source is **not bind-mounted** — the front stack runs the prebuilt `dementev/fars:latest` image, so editing source here has no effect on the running stack until you rebuild the image:

```bash
cd ~/Clients/Citimarine/_code/fars
make test && docker build --network host -t dementev/fars:latest .
cd ../infrastructure/citimarine/local && make up      # recreates only the changed container
```

What the umbrella does mount into `fars-1` (`emit_fars_mounts` in `just umbrella-mounts`, front stack): the theme at `/app/data/images/themes/citimarinestore:ro` and every cloned module at `/app/data/images/modules/<name>:ro`, because FARS reads originals from there. A new module's images are invisible to FARS (every resize 404s) until `just umbrella-mounts` is rerun and the stack recreated.

The PrestaShop-side integration (Smarty helpers that emit `/resize/{w}x{h}/{path}` URLs) lives
in a separate module, `modules/cm_fars` — a change to image *URLs* usually belongs there, not
here. Angie routes `/resize/…` (only through the named location `@resize_backend`) to this service on port 9090. Metrics are on by default at `/metrics` on the API listener; infra's `configs/fars/fars.config.yaml` moves them to a separate listener on 9180, which Prometheus scrapes (job `ctm_fars`).
