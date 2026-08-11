# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

No Go toolchain is installed on the WSL host. Docker is, so run tooling in a container — or use the devcontainer (`.devcontainer/devcontainer.json`), which ships the Go toolchain, Node LTS, and Claude Code itself, with `~/.claude` on a named volume so a login survives rebuilds:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm \
  sh -c 'gofmt -l . && go vet ./... && go test -race -cover ./...'
```

Use the `bookworm` image, not `alpine`, for anything with `-race`: the race detector needs cgo and a C toolchain, which `golang:*-alpine` lacks (`go: -race requires cgo`).

`go build` in that bind-mounted container fails with `error obtaining VCS status` (the container user doesn't own the mounted `.git`); add `-buildvcs=false`. The Dockerfile build is unaffected — `.dockerignore` excludes `.git`.

```bash
go build -o adguard-sidecar .                 # builds all non-test .go files
go test -run TestApplySmartRule ./...         # single test
go test -run 'TestApplySmartRule/Migrate_legacy_regex' ./...  # subtest (spaces → underscores)
docker build -t adguard-sidecar .
docker buildx build --platform linux/amd64,linux/arm64 .   # what release builds
docker compose up -d && docker compose logs -f adguard-sidecar
```

## Architecture

Single-package (`package main`) sidecar, **Go standard library only** — `go.mod` has zero dependencies and should stay that way. Two source files:

- `main.go` — config, health server, poll loop, and the rule reconciliation logic (`generateSmartRule`, `applySmartRule`).
- `k8s.go` — `K8sClient` interface with two implementations: `ClusterClient` (raw REST against `KUBERNETES_SERVICE_HOST` using the mounted service-account token + CA) and `MockClient` (returned automatically when `KUBERNETES_SERVICE_HOST` is unset, i.e. local/dev, and returns an empty host list). The interface exists so tests can substitute hosts without a cluster.

### Reconciliation model

Each `CHECK_INTERVAL` tick, `syncRules` does: list Ingress hosts from the K8s API → `GET /control/filtering/status` for AdGuard's `user_rules` → rebuild the list → `POST /control/filtering/set_rules` only if the result differs. Both AdGuard calls use HTTP basic auth and a 10s timeout.

The sidecar owns a **sentinel-delimited managed block** at the end of `user_rules`:

```
! -- ADGUARD EXTERNAL DNS SIDECAR START --
||*.<root>^$dnsrewrite=NOERROR;A;<fallbackIP>,denyallow=host1|host2
! -- ADGUARD EXTERNAL DNS SIDECAR END --
```

Key invariants in `applySmartRule`:

- Everything between the sentinels is **discarded and regenerated** — never hand-edit content there.
- Rules outside the block are preserved in order, but the managed block is always re-appended at the end, so rules that were below it move above it.
- Only hosts ending in `.<ROOT_DOMAIN>` become `denyallow=` entries; other Ingress hosts are ignored.
- Legacy-rule cleanup (bare `||*.<root>^…` wildcards and old `/regex/…$dnsrewrite` rules) runs **only until a sentinel is seen** — it migrates pre-sentinel deployments and must not strip rules once a managed block exists.

The `denyallow` approach replaced an earlier regex/exception scheme (commit 64aa091); prefer standard AdGuard wildcard + modifier syntax over regex rules.

### Health checks

`healthy` and `lastCheckOK` are `atomic.Bool` — written by the sync loop, read by the health server goroutine. Keep them atomic; CI runs `go test -race`.

`/readyz` is always 200; `/healthz` returns 503 while the last sync failed. That asymmetry is intentional and documented in the README: `/healthz` must not be a liveness probe, or an AdGuard outage restarts the sidecar in a loop. The same binary doubles as its own probe via the `-health` flag (`main.go:19`), which is what the Docker `HEALTHCHECK` invokes — no shell or curl exists in the `scratch` image.

## Conventions

- Fail fast on bad config (`log.Fatalf` in `loadConfig`), but never crash on a failed sync — log, mark unhealthy, and retry next tick.
- Never log `ADGUARD_PASS` or the service-account token.
- Tests must not hit a live AdGuard; use `httptest` (see `TestUpdateUserRules_ContentTypeHeader`) or the `K8sClient` mock.
- **The Go version has one source of truth: the `go` directive in `go.mod`.** `setup-go` reads it via `go-version-file`, and a CI step fails the build if the root `Dockerfile` builder tag disagrees. When bumping, change `go.mod` + `Dockerfile` together. The devcontainer deliberately pins **no** Go version: its image lags upstream (`1-1.24` is the newest published tag) and `GOTOOLCHAIN=auto` fetches whatever `go.mod` asks for.
- **`.devcontainer/Dockerfile` exists for two fixes, both verified by `devcontainer up`:**
  1. The upstream Go image ships an apt source for yarn whose signing key is missing (`NO_PUBKEY 62D54FD4003F6525`), so `apt-get update` — and therefore *every* feature install — fails with exit 100. It deletes `/etc/apt/sources.list.d/yarn.list`.
  2. The image hard-sets `GOTOOLCHAIN=local`, which makes any command fail with `go.mod requires go >= 1.26 (running go 1.24.5; GOTOOLCHAIN=local)`. It sets `GOTOOLCHAIN=auto`.

  Don't "simplify" this back to a plain `"image"` key without re-running `devcontainer up`. Note `go version` still prints 1.24.5 inside the container — that's the launcher; `go list -f '{{.Module.GoVersion}}'` and the actual build use the downloaded 1.26 toolchain.
- **`ARG TARGETOS`/`ARG TARGETARCH` must have no default values.** A default wins over BuildKit's automatic value and silently produces an amd64 binary inside the arm64 image. The builder stage runs on `--platform=$BUILDPLATFORM` and cross-compiles.
- Container runs as UID 65534 from `scratch`; only `zoneinfo`, CA certs, and the binary are present.
- `.dockerignore` excludes `*_test.go`, so `COPY *.go ./` + `go build .` in the Dockerfile pick up new source files automatically — no file list to maintain.

## CI / release flow

`ci.yml`, `cd.yml`, and `auto-release.yml` all delegate to the reusable `build.yml`, so CI and release builds share one code path:

- **`push: false` (CI)** — lint/vet/`go test -race`, Go-version-consistency check, then a single-arch image build that is `load`ed and **smoke tested**: no-config run must exit non-zero, and `/readyz` must answer `READY` from the running `scratch` container.
- **`push: true` (release)** — multi-arch (`linux/amd64,linux/arm64`) build pushed to `ghcr.io/<repo>`.

What CI still cannot exercise is everything gated on `inputs.push`: `docker/login-action`, the registry push, and the `type=semver` tags `metadata-action` only computes when a version is passed. Treat majors of those actions as release-risk.

`auto-release.yml` runs **hourly** with `default_bump: false`, so **only conventional-commit subjects (`fix:`, `feat:`, …) produce a tag** — `chore:` ships nothing. The tag is pushed with `GITHUB_TOKEN`, which does not trigger `cd.yml`; the publish happens in that same Auto Release run's `release` job.

`renovate.json` groups all GitHub Actions bumps into one PR (6 of 8 action refs live in `build.yml`, so separate PRs conflict) and groups the three Go version sites into one. See `.claude/skills/merge-renovate-prs/SKILL.md` for the triage/merge procedure.

## Known gaps

- `README.md` claims MIT but there is **no `LICENSE` file**.
- Test coverage is ~29%: `loadConfig`, the health handlers, and `ClusterClient` are untested.
