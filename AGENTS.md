# AGENTS.md

Small single-binary Go CLI that garbage-collects images from a Docker Distribution registry. Two files do the work: `main.go`, `types.go`. End-to-end tests live under `integration/` and exercise the built binary against an in-memory OCI v2 registry.

## Commands

CI (`.github/workflows/build.yml`) runs exactly these, in order, on push/PR to `master`:

```
go mod download
go mod verify
go vet ./...
go build -v ./...
# `go test -race -v ./...` only if *_test.go exists (none today)
```

Match this locally before pushing. There is no Makefile, no linter config, no formatter config beyond `gofmt`/`go vet` defaults.

Go version is pinned via `go.mod` (`go 1.26.0`) and CI uses `go-version-file: go.mod` with `check-latest: true` — do not downgrade the directive without coordinating with CI.

## Running the tool

```bash
go run .            # prints usage (no args = usage, not an error)
go run . -dry -registry http://localhost:5000 -repos 30
```

Version string is hardcoded in `main.go` (`const version = "0.10.2"`). Bump it there and update `CHANGELOG.md` when releasing. See the "Release workflow" section below for the full cut-release procedure.

## Architecture (read before editing `main.go`)

Flow per invocation:

1. Build a plain `http.Transport` (`http.ProxyFromEnvironment` + `tls.Config{InsecureSkipVerify: *insecure}`) and an `authn.Authenticator` (`authn.Anonymous` by default, `&authn.Basic{Username, Password}` when `-user`/`-password` are set). Plug both into go-containerregistry via `remote.WithTransport(...)` + `remote.WithAuth(...)`. go-containerregistry's own `basicTransport` (pkg/v1/remote/transport/basic.go) host-gates the `Authorization` header against the registry host, so credentials never leak to auth-token redirects.
2. List repositories via `remote.CatalogPage(registry, last, windowSize, opts...)`. `-paginate` controls the loop: without it, one fetch of `-repos` size; with it, pages of `-page-size` until the server returns a short page or `len(entries) >= -repos`.
3. For each repo matching `-repo` regex, use `remote.List(repoRef, opts...)` for tag names, then per tag `remote.Get(repoRef.Tag(tag), opts...)` for the descriptor and `desc.Image().ConfigFile()` for the `Created` timestamp. `ConfigFile()` reads both Docker schema2 and OCI image configs. If `desc.MediaType.IsIndex()` is true the tag is a multi-arch manifest list and the whole repo is skipped (no single `Created` timestamp exists). Any error on any tag skips the whole repo — this is intentional to avoid deleting live images on partial data. Do not "fix" this by continuing on error.
4. Sort tags oldest-first (`ImageByDate` in `types.go`) and walk newest→oldest. `-latest N` preserves the N most-recent matching tags. `-day/-month/-year` combine into a single deadline via `time.Now().AddDate(-year, -month, -day)`.
5. Deletion is two-phase and non-obvious:
   - **Phase 1 (shared digests):** Docker Distribution has no `Untag`. If a deletable tag shares its digest with a non-deletable tag, the code retags the deletable tag to a *different* deletable tag's digest (the "replacement digest" pulled from `replacementDigests`), then deletes that replacement digest. Retagging uses `desc, _ := remote.Get(srcDigestRef, opts...)` + `remote.Tag(dstTagRef, desc, opts...)` — the descriptor is `remote.Taggable`, so `remote.Tag` writes its manifest bytes under the new tag name without re-uploading blobs. This effectively removes the outdated tag without destroying the image the preserved tag points to. If no disposable replacement is available, the tag is skipped with a log.
   - **Phase 2 (non-shared digests):** plain `remote.Delete(repoRef.Digest(digest), opts...)`.

   When changing deletion logic, preserve both phases and the `digestsDeleted`/`replacementDigests` bookkeeping — getting this wrong deletes images that are still tagged.
6. `-dry` short-circuits the `remote.Tag`/`remote.Delete` calls but still runs all the analysis and logging.

## Conventions / gotchas

- Registry server requires `delete.enabled: true`; otherwise every delete 405s. The README covers this.
- All logging uses `sirupsen/logrus` with `log.WithField(...)`. Keep structured fields; do not switch to `fmt.Printf`.
- Log streams are split by level: `Info`/`Debug`/`Trace` → stdout, `Warn`/`Error`/`Fatal`/`Panic` → stderr. Wired in `initLogging()` (called from `main()` after the `-v` short-circuit) via `logrus/hooks/writer`: the default writer is redirected to `io.Discard` and two `writer.Hook` instances handle disjoint level sets. `log.Fatalf` still exits non-zero because logrus fires hooks before `os.Exit`. Wire format (logfmt, field order, timestamp) is unchanged — only the destination fd differs. Pinned by `integration/log_streams_test.go`. When adding new log calls, just pick the appropriate level; routing happens automatically. Do not reintroduce `log.SetOutput(os.Stderr)` or the equivalent — it would silently collapse the split.
- `-user` without `-password` prompts on stdin via `golang.org/x/term` — keep this interactive path working (no hard requirement on a TTY unless user opted in).
- Flags are package-level pointers initialized in `init()`. Adding a flag means: declare pointer in the `var (...)` block, register it in `init()`, document it in `README.md`'s Arguments list.
- Registry HTTP API client is `github.com/google/go-containerregistry` (`pkg/name`, `pkg/v1/remote`, `pkg/authn`). The older `github.com/docker/distribution` client is intentionally NOT a dependency — it was removed from distribution/distribution v3.0.0's public API (distribution/distribution#4126) and the ecosystem standardised on go-containerregistry (crane, ko, cosign, Harbor).
- Basic auth goes through `authn.Basic` passed to `remote.WithAuth`. go-containerregistry's internal `basicTransport` sets `Authorization: Basic ...` only when `req.Host == registry.Host`, so the same safety property a bespoke URL-gating transport would give us is provided by the library. Pinned by `integration/basicauth_test.go` (correct creds succeed, wrong password → 401, no creds → 401).
- `-registry` URLs with a path component (e.g. `https://example.com/docker`) are rejected in `parseRegistryHost` at start-up. `go-containerregistry`'s `name.NewRegistry` only accepts an RFC 3986 authority and every `remote.*` call hardcodes `/v2/` at the host root, so there is no way to honour a path prefix without either a workaround transport (explicitly decided against — too messy for what is essentially an upstream constraint) or changes in `go-containerregistry` itself. Rejection is pinned by `integration/registry_url_test.go`.
- Dependabot (`.github/dependabot.yml`) handles gomod + github-actions weekly; prefer letting it bump versions rather than manual upgrades.

## Contributing

This checkout is the `aviationexam/deckschrubber` fork. Two remotes are configured:

- `origin` → `aviationexam/deckschrubber` (our fork — target all PRs here)
- `upstream` → `fraunhoferfokus/deckschrubber` (original project)

**Open pull requests against `origin` only.** We treat this fork as our own sandbox and iterate freely here. Please don't send PRs upstream from this checkout — we'd like to keep a friendly relationship with the upstream maintainer, and coordinating changes back to them is handled separately and deliberately.

## Release workflow

Cutting a new release is a single-PR + tag + GitHub-Release flow. Publishing the Release is what triggers the signed image push to GHCR (`.github/workflows/release.yml`), so the Release is not cosmetic — it is the deploy trigger.

### Procedure

1. **Start from up-to-date master.**
   ```
   git checkout master
   git pull origin master
   git checkout -b release/<version>          # e.g. release/0.9.1
   ```

2. **Bump the version in exactly two files** (do NOT bump anywhere else — there is no other source of truth):
   - `main.go`: `const version = "<new version>"` (no `v` prefix).
   - `CHANGELOG.md`: new top entry `` `<new version>` (aviationexam fork): `` with a sub-bullet per user-visible change since the previous tag. Link PRs as `[#N](https://github.com/aviationexam/deckschrubber/pull/N)`. Keep entries concise but specific (what changed, why it matters).
   - Also update the `const version = "..."` reference in this `AGENTS.md` so it stays in sync with `main.go`.

3. **Run the CI pipeline locally** (must be clean before opening the PR):
   ```
   go mod download
   go mod verify
   go vet ./...
   go build -v ./...
   ```

4. **Commit, push, open PR.**
   - Commit message: `release: <version>` (matches the `release: 0.9.0 (#14)` precedent).
   - PR title: `release: <version>`.
   - PR body: list headline change(s), other changes since last release, release mechanics (files touched + old→new version), and the exact verification commands you ran.
   - PR target: `origin/master` only (see Contributing above).

5. **Merge the PR** (squash or merge commit — match recent history; `release: 0.9.0 (#14)` was squash-merged).

6. **Tag master and push the tag.** The tag is what `go install …@<tag>` resolves against and what the Docker workflow embeds via `DECKSCHRUBBER_VERSION`, so tag the exact merge commit on `master`:
   ```
   git checkout master
   git pull origin master
   git tag -a v<version> -m "v<version>"      # e.g. v0.9.1
   git push origin v<version>
   ```
   Use an annotated tag (`-a`) with a `v` prefix — `go install` and `metadata-action`'s `type=semver` both expect `v<semver>`.

7. **Publish the GitHub Release.** This is the action that fires the `release.yml` workflow and produces the signed GHCR image; do not skip it.
   ```
   gh release create v<version> \
     --repo aviationexam/deckschrubber \
     --title "v<version> - <short headline>" \
     --notes-file <path-to-notes.md>
   ```
   Release notes structure (see `v0.9.0` release for the canonical shape):
   - Opening paragraph referencing the fork.
   - `## Headline change` — the one thing this release is about.
   - `## Other changes since <previous tag>` — bulleted, PR-linked.
   - `## Install` — `go install github.com/aviationexam/deckschrubber@v<version>`.
   - `**Full diff:** [\`v<prev>...v<version>\`](https://github.com/aviationexam/deckschrubber/compare/v<prev>...v<version>)`.
   - For stable releases leave `--prerelease` off so `latest` image tag moves. For pre-releases (e.g. `-rc.1`) pass `--prerelease` — `metadata-action` auto-skips `latest` for those.

8. **Verify the release workflow succeeded.**
   ```
   gh run list --workflow=release.yml --repo aviationexam/deckschrubber --limit 1
   ```
   Image should appear at `ghcr.io/aviationexam/deckschrubber:<version>` (and `:latest` for stable releases), with a cosign signature verifiable via the recipe in the PR #16 description.

### Release anti-patterns

- Don't tag before the release PR is merged — the tag would not contain the version bump.
- Don't create the GitHub Release without a tag, or with a tag that doesn't match `v<semver>` exactly — the Docker workflow reads `github.event.release.tag_name` verbatim.
- Don't bump the version in `main.go` without a matching `CHANGELOG.md` entry (or vice versa).
- Don't skip publishing the GitHub Release thinking "the tag is enough" — no Release event, no image.

## What not to do

- Don't add tests-as-a-side-effect of unrelated PRs; there are none and CI conditionally skips the step. If you add any `*_test.go`, CI will start running `go test -race -v ./...` and must pass.
- Don't introduce a vendored directory; the test step explicitly excludes `./vendor/*` but nothing else in the repo assumes vendoring.
- Don't replace the two-phase retag-and-delete with a naive single delete loop (see Architecture §5).
- Don't swallow per-tag fetch errors; the current "skip whole repo" behavior is a safety feature.
