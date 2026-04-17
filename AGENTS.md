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

Go version is pinned via `go.mod` (`go 1.24.0`) and CI uses `go-version-file: go.mod` with `check-latest: true` — do not downgrade the directive without coordinating with CI.

## Running the tool

```bash
go run .            # prints usage (no args = usage, not an error)
go run . -dry -registry http://localhost:5000 -repos 30
```

Version string is hardcoded in `main.go` (`const version = "0.8.0"`). Bump it there and update `CHANGELOG.md` when releasing.

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

This checkout is the `aviationexam/fraunhoferfokus-deckschrubber` fork. Two remotes are configured:

- `origin` → `aviationexam/fraunhoferfokus-deckschrubber` (our fork — target all PRs here)
- `upstream` → `fraunhoferfokus/deckschrubber` (original project)

**Open pull requests against `origin` only.** We treat this fork as our own sandbox and iterate freely here. Please don't send PRs upstream from this checkout — we'd like to keep a friendly relationship with the upstream maintainer, and coordinating changes back to them is handled separately and deliberately.

## What not to do

- Don't add tests-as-a-side-effect of unrelated PRs; there are none and CI conditionally skips the step. If you add any `*_test.go`, CI will start running `go test -race -v ./...` and must pass.
- Don't introduce a vendored directory; the test step explicitly excludes `./vendor/*` but nothing else in the repo assumes vendoring.
- Don't replace the two-phase retag-and-delete with a naive single delete loop (see Architecture §5).
- Don't swallow per-tag fetch errors; the current "skip whole repo" behavior is a safety feature.
