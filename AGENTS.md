# AGENTS.md

Small single-binary Go CLI that garbage-collects images from a Docker Distribution registry. Three files do the work: `main.go`, `types.go`, `util/basicauthtransport.go`. No tests.

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

Version string is hardcoded in `main.go` (`const version = "0.9.3"`). Bump it there and update `CHANGELOG.md` when releasing. See the "Release workflow" section below for the full cut-release procedure.

## Architecture (read before editing `main.go`)

Flow per invocation:

1. Build an `http.Transport` with optional basic auth via `util.BasicAuthTransport` (also handles `-insecure` TLS and `http.ProxyFromEnvironment`). Basic auth is only applied when the request URL starts with the configured registry URL.
2. List repositories via `client.NewRegistry(...).Repositories(ctx, buf, last)`. `-paginate` controls the loop: without it, one fetch of `-repos` size; with it, pages of `-page-size` until `io.EOF` or `len(entries) >= -repos`.
3. For each repo matching `-repo` regex, fetch every tag, its manifest, and the config blob (`schema2.DeserializedManifest` → `BlobInfo.Created`). Any error on any tag skips the whole repo — this is intentional to avoid deleting live images on partial data. Do not "fix" this by continuing on error.
4. Sort tags oldest-first (`ImageByDate` in `types.go`) and walk newest→oldest. `-latest N` preserves the N most-recent matching tags. `-day/-month/-year` combine into a single deadline via `time.Now().AddDate(-year, -month, -day)`.
5. Deletion is two-phase and non-obvious:
   - **Phase 1 (shared digests):** Docker Distribution has no `Untag`. If a deletable tag shares its digest with a non-deletable tag, the code retags the deletable tag to a *different* deletable tag's digest (the "replacement digest" pulled from `replacementDigests`), then deletes that replacement digest. This effectively removes the outdated tag without destroying the image the preserved tag points to. If no disposable replacement is available, the tag is skipped with a log.
   - **Phase 2 (non-shared digests):** plain `manifestService.Delete(ctx, digest)`.

   When changing deletion logic, preserve both phases and the `digestsDeleted`/`replacementDigests` bookkeeping — getting this wrong deletes images that are still tagged.
6. `-dry` short-circuits the `Tag`/`Delete` calls but still runs all the analysis and logging.

## Conventions / gotchas

- Registry server requires `delete.enabled: true`; otherwise every delete 405s. The README covers this.
- All logging uses `sirupsen/logrus` with `log.WithField(...)`. Keep structured fields; do not switch to `fmt.Printf`.
- `-user` without `-password` prompts on stdin via `golang.org/x/term` — keep this interactive path working (no hard requirement on a TTY unless user opted in).
- Flags are package-level pointers initialized in `init()`. Adding a flag means: declare pointer in the `var (...)` block, register it in `init()`, document it in `README.md`'s Arguments list.
- `go.mod` intentionally uses `github.com/docker/distribution v2.8.3+incompatible` and pairs it with `github.com/distribution/reference`. The two packages are split; don't "simplify" by importing only one.
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
