# FARS — Fast Auto Resize Service

> Part of the **Citimarine umbrella workspace**. See `../CLAUDE.md` for the full topology and rules.

## What this repo is

Go source for FARS, the image-resize service Citimarine uses to generate thumbnail/responsive variants on demand. Built into the `dementev/fars:latest` Docker image, which runs as the `fars-1` service in the local front stack.

- Remote: `git@github.com:levskiy0/FARS.git` (**GitHub**, not GitLab — the only repo in the umbrella hosted on GitHub)
- Default branch: `main`

## Layout

`cmd/fars-server` (entrypoint) · `internal/` (implementation) · `pkg/` (exported packages) ·
`tests/` (fixture images + cache) · `example.config.yaml` (copy to `config.yaml` to run).

## Commands

Go 1.27. Use the `Makefile`, not raw `go` invocations:

```bash
make build     # → ./fars binary, version stamped from `git describe`
make run       # build + serve --config ./config.yaml (override: CONFIG=…)
make test      # go test ./...  (GOCACHE is pinned to ./.gocache)
make fmt       # gofmt -w ./cmd ./internal ./pkg ./tests
make clean     # remove the binary
```

## Local dev integration

FARS is **not bind-mounted** — the front stack runs the prebuilt `dementev/fars:latest` image,
so editing Go source here has no effect on the running stack until you rebuild the image:

```bash
cd ~/Clients/Citimarine/_code/fars
make test && docker build -t dementev/fars:latest .
cd ../infrastructure/citimarine/local && make down && make up
```

The PrestaShop-side integration (Smarty helpers that emit `/resize/{w}x{h}/{path}` URLs) lives
in a separate module, `modules/cm_fars` — a change to image *URLs* usually belongs there, not
here. Angie routes `/resize/…` to this service on port 9090.
