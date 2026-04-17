# CHANGELOG
* `0.9.1` (aviationexam fork):
  * Publish signed container images to `ghcr.io/aviationexam/deckschrubber` on every GitHub Release via a new `release.yml` workflow; images are built from `go install github.com/aviationexam/deckschrubber@<tag>` inside a multi-stage `golang:1.25-alpine3.23` → `alpine:3.23` Dockerfile and signed with cosign (keyless, GitHub OIDC → Fulcio → Rekor) ([#16](https://github.com/aviationexam/deckschrubber/pull/16))
  * Track Dockerfile base images with Dependabot (`docker` ecosystem entry) so `golang`/`alpine` base tags get weekly bump PRs
  * Fix auto-format workflow skipping commits when only subdirectory Go files changed: set `disable_globbing: true` on `git-auto-commit-action` so `*.go` is interpreted as a git pathspec matching every nested package, not a shell glob against repo root ([#15](https://github.com/aviationexam/deckschrubber/pull/15))
* `0.9.0` (aviationexam fork):
  * Safe untag for shared digests via two-phase deletion: tags sharing a manifest with a preserved tag are now retagged to a disposable digest before deletion, avoiding accidental removal of still-tagged images ([#2](https://github.com/aviationexam/deckschrubber/pull/2))
  * Fix pagination panic: pass allocated buffer to `Repositories()` instead of an empty slice
  * Rename module to `github.com/aviationexam/deckschrubber` ([#12](https://github.com/aviationexam/deckschrubber/pull/12))
  * Drop deprecated `docker/distribution/context` in favor of the stdlib `context`
  * Add CI build workflow, Dependabot, and auto-format workflow ([#5](https://github.com/aviationexam/deckschrubber/pull/5))
  * Add repo-specific agent guidance (`AGENTS.md`) ([#4](https://github.com/aviationexam/deckschrubber/pull/4))
  * Bump Go toolchain and dependencies (logrus, golang.org/x/term, prometheus/common, prometheus/procfs, yaml/v2, GitHub Actions)
* `0.8.0`:
  * Update dependencies
  * Add pagination (thanks to [@aoresnik](https://github.com/aoresnik))
  * Update README
* `0.7.0`:
  * Fix logrus issue (name case)
  * Strucured as Go module
  * Add compact logged list of fetched repos
  * Friendlier API (usage on no args!)
* `0.6.0`: Add basic authentication
* `0.5.0`: Add `ntag` flag to match "everything but this" tags
* `0.4.0`:
  * Corrected behavior with images that have more than one tag (bug #9)
  * Changed the meaning of time-limit (e.g `-day`) in combination with `-latest` flag: it only takes into account whichever means more preserved matching tags
* `0.3.0`: Adapt to new Docker Distribution API
* `0.2.0`:
  * Null pointer bug fix
  * Additional features (match by tag/repo, custom latest ignore, debug mode)
* `0.1.0`: First working draf
  * Works only with `http`
  * Number of repositories can be limited
  * Age in years, months, days, or a combination
  * Allows dry run