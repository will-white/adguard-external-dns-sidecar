---
name: merge-renovate-prs
description: >-
  Triage and merge this repo's open dependency PRs (Renovate bumps for GitHub
  Actions, the golang builder image, the Go directive, and the devcontainer)
  safely, in bulk. Use when the user asks to "go through the PRs", "merge the
  PRs", "clear the Renovate backlog", "update dependencies", or similar. Handles
  the build.yml pile-up, duplicate major/minor PRs, researches breaking changes
  on majors, holds changes only the release path exercises, and knows that a
  `fix:`-titled merge cuts a release within the hour.
---

# Merge Renovate / dependency PRs

Repo: **`will-white/adguard-external-dns-sidecar`** — a single-package, **zero-dependency**
Go sidecar (see `CLAUDE.md`). Goal: merge everything safe, resolve duplicates to the
highest appropriate version, apply any code change a major requires, and **stop and ask**
before anything that only breaks at release time.

Two facts shape every decision here:

- **`go.mod` has no `require` block.** There are no Go module dependencies and stdlib-only
  is a deliberate constraint. So the entire class of "module major needs a code refactor"
  does not exist — and a Renovate PR that *adds* a module is a red flag, not a routine bump
  (§5.3).
- **Merging to `main` can ship a release on its own.** `auto-release.yml` runs **hourly**
  with `default_bump: false`: a merged commit titled `fix:`/`feat:` gets tagged and the
  image published to `ghcr.io` within the hour, with no further action from you. Renovate
  titles bumps `chore(deps): …`, which cuts nothing — but check the title before merging,
  and see §5.1.

## 0. Preflight

- **`gh` is not installed in Linux. Use `gh.exe`** (Windows GitHub CLI, reachable from WSL
  at `/mnt/c/Program Files/GitHub CLI/gh.exe`, already on `PATH`).
- **Always pass `--repo will-white/adguard-external-dns-sidecar`.** `gh.exe` cannot
  autodetect the repo — it shells out to Windows `git`, which rejects the WSL path and
  prints only a `git config --global --add safe.directory '%(prefix)///wsl.localhost/…'`
  suggestion. Every bare `gh.exe pr list` fails this way. Do not "fix" it with a global
  `safe.directory`; just pass `--repo`.
- Confirm auth: `gh.exe api user -q .login` → `will-white`. Scopes need `repo` and
  `workflow` — most PRs here edit `.github/workflows/*`.
- `main` is **unprotected** (`gh.exe api repos/<repo>/branches/main/protection` → 404), so
  red checks do not block merging. That is a licence to use judgment, not to ignore CI.
- Repo merge settings: **squash, rebase, and merge commits are all enabled**; auto-merge is
  off; branches auto-delete. **Use `--squash`** to keep history linear and the changelog
  readable — the release tool reads commit subjects.
- Prefer `--body '...'` inline over `--body-file`; `gh.exe` reads paths as Windows paths and
  a WSL path may not resolve.

## 1. Is Renovate even active yet?

`renovate.json` now lives on `main` with grouping rules (see below), which **supersedes
Renovate's onboarding PR #1 `chore: Configure Renovate`** (branch `renovate/configure`,
opened 2026-03-02). Renovate closes that PR itself once it detects config on the default
branch; if it is still open and the config has landed, close it with a note rather than
merging a duplicate bare config.

**Renovate opens no dependency PRs until config is on `main`.** If neither the config nor a
merged onboarding PR exists, there is no backlog to clear — say so and stop.

The committed config groups bumps deliberately, which changes what you should expect to see:

- **All GitHub Actions bumps arrive as one PR** (`groupName: github actions`) — because 6 of
  the 8 action refs live in `build.yml` and separate PRs conflict with each other (§3).
- **All three Go version sites arrive as one PR** (`groupName: go version`) — `go.mod`, the
  `Dockerfile` builder tag, and the devcontainer image. CI fails if the first two disagree,
  so they must move together.
- The `docker-compose` manager is disabled (its `image:` is a locally built tag).

A grouped PR is still one merge, but it touches several files at once — read the whole diff
rather than assuming a one-line bump.

## 2. Survey every open PR

```bash
R=will-white/adguard-external-dns-sidecar
gh.exe pr list --repo $R --state open --limit 100 \
  --json number,title,mergeable,mergeStateStatus,isDraft,author \
  --jq '.[] | "\(.number)\t\(.author.login)\t\(.mergeable)\t\(.mergeStateStatus)\t\(.title)"' | sort -n
```

Then the **file-conflict map**, which drives merge order:

```bash
for pr in <all numbers>; do
  echo "$pr :: $(gh.exe pr view $pr --repo $R --json files --jq '[.files[].path]|join(" ")')"
done
```

**Checks should be green, not `UNSTABLE`.** This repo has no `govulncheck` or security
workflow — the only checks are `Build (no push) / Lint and Test` (Go version consistency,
gofmt, `go vet`, `go test -race -cover`) and `Build (no push) / Build Docker Image` (build +
load + smoke test), both from the reusable `build.yml`. A red check here is real and caused
by the PR. Two failures have obvious causes worth recognising: **Check Go version
consistency** means a partial Go bump (§3), and a **Smoke test** failure means the built
image no longer starts. Investigate, don't wave through:

```bash
gh.exe pr checks <pr> --repo $R
```

## 3. Classify

Renovate's whole surface in this repo (`config:recommended` enables the github-actions,
dockerfile, docker-compose, devcontainer and gomod managers):

| Where | What gets bumped |
|---|---|
| `.github/workflows/build.yml` | `actions/checkout@v4` ×2, `actions/setup-go@v5`, `docker/setup-qemu-action@v3`, `docker/setup-buildx-action@v3`, `docker/login-action@v3`, `docker/metadata-action@v5`, `docker/build-push-action@v5` |
| `.github/workflows/auto-release.yml` | `actions/checkout@v4`, `mathieudutour/github-tag-action@v6.2` |
| `Dockerfile` | `golang:1.26-alpine3.24` builder tag |
| `go.mod` | the `go 1.26` directive (there is no `toolchain` line) |
| `.devcontainer/devcontainer.json` | `mcr.microsoft.com/devcontainers/go:1-1.24-bookworm`, docker-in-docker feature |

Notes that matter:

- **All actions use plain `@vN` tags — nothing is SHA-pinned**, so there are no
  SHA+comment pairs to keep in sync.
- **`build.yml` is the pile-up file**: 7 of the 9 action refs live there. Grouping (§1)
  normally prevents same-file conflicts, but they return the moment a bump is split out.
- **`actions/checkout` spans 3 lines across 2 files** (twice in `build.yml`, once in
  `auto-release.yml`) — verify all three moved.
- **True duplicates** = two PRs editing the **same line** for the same dependency. Renovate
  routinely opens a minor *and* a major (e.g. `docker/build-push-action v5.5.0` alongside
  `v6`). Keep the **highest** version §4 clears; **close the other** with a note naming the
  PR that superseded it.
- **Go version PRs are a set, not duplicates** — and CI enforces it. `go.mod` is the single
  source of truth: `setup-go` reads it via `go-version-file`, and a **Check Go version
  consistency** step fails the build when the `Dockerfile` builder tag disagrees with the
  `go` directive. So a partial Go bump goes red rather than silently drifting; fix it by
  bumping both in the same branch. The **devcontainer image lags upstream** (latest is
  `1-1.24` while `go.mod` targets 1.26) — that gap is deliberate and closed by
  `GOTOOLCHAIN=auto` in `.devcontainer/Dockerfile`, so do not "fix" it by lowering `go.mod`.
  A devcontainer image bump is safe to merge on its own; it pins no Go version.
- **Non-Renovate PRs** — review individually, never batch.

## 4. Research breaking changes on majors

Fan out one general-purpose agent per major (parallel). Each must read the affected
workflow, fetch upstream release notes, and cross-reference **the inputs this repo actually
sets** against removed/renamed ones, reporting `file:line` edits needed. Specific to this
repo:

- **`docker/build-push-action`** — `cache-from`/`cache-to: type=gha` with a
  `scope=${{ hashFiles(...) }}`, and `outputs.digest`, which `build.yml` exports as a
  workflow output.
- **`docker/metadata-action`** — the `tags:` block leans hard on `type=semver` with
  `value=${{ inputs.version }}` and `enable=` conditions, plus `type=raw,value=latest`.
  Schema changes here break tagging **silently** (§5.1).
- **`mathieudutour/github-tag-action`** — `default_bump: false` and
  `default_prerelease_bump: false` are load-bearing (they're why routine `chore:` merges
  don't cut releases); confirm both inputs survive, and that `outputs.new_tag` /
  `outputs.changelog` keep their names.
- **`actions/checkout`** — `ref: ${{ inputs.version || github.ref }}` in `build.yml` and
  `fetch-depth: 0` in `auto-release.yml` must survive.
- **`actions/setup-go`** — `cache: false` is set deliberately (nothing to cache with zero
  deps).
- **Builder-image majors** (`golang:1.24-alpine…`) — also bump `go.mod` / `build.yml:49` per
  §3, and remember `CLAUDE.md`: the Dockerfile's `COPY` and `go build` lines name
  `main.go k8s.go` explicitly.

## 5. HOLD and ASK (do not auto-merge)

Surface with `AskUserQuestion` before merging:

1. **Anything touching the release-only half of `build.yml`.** CI builds the image on every
   PR (`ci.yml` → `build.yml` with `push: false`), **loads it, and smoke tests it** — a
   no-config run must exit non-zero and `/readyz` must answer from the running container. So
   buildx, build-push-action and the `Dockerfile` are genuinely exercised pre-merge,
   including that the `scratch` image still executes.

   What CI never runs is everything gated on `inputs.push`: `docker/login-action`, the
   registry push, the **multi-arch** build (`platforms:` is only set when pushing, and QEMU
   is only installed then), and the `type=semver` tags `metadata-action` computes only when a
   version is passed. A major in `login-action`, `metadata-action`, or `setup-qemu-action`
   can pass every check and then fail — or mistag — at release time. For a suspicious one,
   reproduce locally: `docker buildx build --platform linux/amd64,linux/arm64 .` and confirm
   the arm64 binary is really arm64 (`file` on a `-o type=local` export) — a wrong
   `ARG TARGETARCH` default has produced amd64-inside-arm64 here before.
2. **A bump whose title is `fix:`/`feat:` rather than `chore:`.** Merging it tags and
   publishes within the hour (§ intro). Confirm the user wants a release, or ask to retitle
   the squash commit to `chore(deps): …` at merge time.
3. **Any PR that adds a `require` to `go.mod`.** Stdlib-only is an intentional constraint;
   treat a new dependency as a design change needing sign-off, not a dependency bump.
4. **Devcontainer majors** — low blast radius (they can't break CI or the image) but they
   change the environment the user actually develops in, and `go` is not on the host PATH,
   so a broken devcontainer means no local toolchain at all.

Everything else — Action minors/patches, `alpine`/patch builder bumps, devcontainer
minors — is safe to batch.

## 6. Merge

```bash
gh.exe pr merge <pr> --repo $R --squash
gh.exe pr view <pr> --repo $R --json state,mergedAt   # verify; gh is quiet on success
```

Order, because of the `build.yml` pile-up:

1. Merge the **Go version set** first (`go.mod` + `Dockerfile` + devcontainer, plus the
   manual `build.yml:49` edit) — it touches files little else touches, and it's the change
   most likely to need a real CI run to trust.
2. Merge the **unique-file** PRs freely.
3. Then work the `build.yml` cluster **one at a time**. GitHub 3-way-merges distinct lines
   cleanly, so siblings usually stay `MERGEABLE`, but after each merge re-check the rest and
   run `gh.exe pr update-branch <pr> --repo $R` on any that flip to `CONFLICTING`.
4. Close the losing half of each duplicate pair with a note naming its replacement.

If Renovate **auto-closes** a PR whose branch conflicted after you merged a same-file
sibling (`state: CLOSED, mergedAt: null`) and the bump is still wanted, recreate it: branch
off `main`, re-apply the one-line change (grab the target version from
`gh.exe pr diff <old-pr> --repo $R`), push, open a PR, merge.

### Gotchas

- Shell is **bash** — normal word-splitting.
- **`go` is not on PATH** in this WSL environment, but **Docker is**, so you can verify a
  branch yourself instead of asking:

  ```bash
  docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm \
    sh -c 'gofmt -l . && go vet ./... && go test -race -cover ./...'
  ```

  Use `bookworm`, not `alpine` — `-race` needs cgo and a C toolchain (`go: -race requires
  cgo` on alpine). The PR's own CI run covers the same ground plus the image build and smoke
  test, so prefer reading it when it is already green.
- Never `git push origin main` — branch, PR, merge, even though main is unprotected.
- There is **no `dependabot.yml`** here; every bot PR should be Renovate. A
  `dependabot[bot]` PR means someone enabled it at the org/account level — check before
  treating it as routine.

## 7. Verify

```bash
gh.exe run list --repo $R --branch main --limit 10 \
  --json workflowName,conclusion,headSha,event \
  --jq '.[] | "\(.conclusion)\t\(.event)\t\(.workflowName)\t\(.headSha[0:7])"'
```

Expect `CI` runs on the merge commits (green) plus the hourly `schedule`-triggered
`Auto Release`. Confirm `Auto Release` is still **success** afterwards — it runs `build.yml`
as a `validate` job before tagging, so it is the closest thing to a release rehearsal, and a
break there is invisible until the next `fix:`/`feat:` commit.

If a release *did* fire, note that the tag is pushed with `GITHUB_TOKEN` (which does not
trigger `cd.yml`) — the publish happens inside that same `Auto Release` run's `release` job,
so check there, not for a separate CD run. Latest tag at time of writing: **v0.3.14**.

## 8. Report

Summarize: merged count, duplicates closed (with which version won), breaking changes fixed,
held PRs **with the reason for each**, and whether the Go version is now consistent across
all four sites. Say explicitly whether a release was triggered, and offer to cut one if the
release path changed but nothing tagged it.
