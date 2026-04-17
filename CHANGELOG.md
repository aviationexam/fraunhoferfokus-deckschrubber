# CHANGELOG
* `0.10.0` (aviationexam fork):
  * Replace the archived `github.com/docker/distribution/registry/client` with `github.com/google/go-containerregistry` as the HTTP client for the Docker Distribution v2 API ([#27](https://github.com/aviationexam/deckschrubber/pull/27)). `distribution/distribution` v3.0.0 removed the client from its public API ([distribution/distribution#4126](https://github.com/distribution/distribution/pull/4126)) and its README redirects consumers away from it; `go-containerregistry` is the ecosystem-standard Go client (crane, ko, cosign, sigstore, Harbor, kpack, imgpkg, kbld) and was already a direct dependency of this repo for the integration test harness. No CLI flag, default, or output changes — `-paginate`, `-page-size`, `-latest`, `-dry`, `-insecure`, `-user`/`-password`, `-debug`, and the two-phase retag+delete algorithm all behave identically.
  * Fix OCI-format images being read as the zero `time.Time`: the previous schema2-only manifest deserialization silently mishandled OCI manifests (`application/vnd.oci.image.manifest.v1+json`), leaving the config digest empty and the `Created` timestamp at `time.Time{}`, which made every OCI-format tag look infinitely outdated. `img.ConfigFile()` now reads the config regardless of wire format. Pinned by `integration/oci_manifest_test.go`.
  * Detect multi-arch manifest lists (`application/vnd.oci.image.index.v1+json` / `application/vnd.docker.distribution.manifest.list.v2+json`) and trigger the existing skip-whole-repo safety instead of letting them fall into the schema2 path. Previously manifest lists shared the same zero-time failure mode as OCI manifests.
  * Pin `util.BasicAuthTransport` URL-prefix-gating with three new integration tests: correct credentials reach the registry, wrong `-password` surfaces a `401`, no credentials surface a `401`. Previously the integration harness ran an open registry and this transport's contract was not tested end-to-end.
  * Preserve path-prefixed `-registry` URLs (`https://example.com/docker` behind a reverse proxy) via a new `util.PathPrefixTransport` that injects the prefix in front of `/v2/...` requests. `go-containerregistry`'s `name.NewRegistry` rejects path components (it validates the argument as an RFC 3986 authority, see `pkg/name/registry.go checkRegistry()`), so an early draft of the migration silently dropped the path, emitting requests to `/v2/...` at the host root and breaking both catalog lookup and `BasicAuthTransport`'s URL-prefix credential gate. Pinned by `integration/path_prefix_test.go` (plain, trailing slash, with basic auth, wrong-prefix negative control) and unit-tested directly in `util/pathprefixtransport_test.go`.
* `0.9.3` (aviationexam fork):
  * Fix Phase 1 shared-digest retag path crashing with `panic("not implemented")`: docker/distribution's client-side `TagService.Tag()` has never been implemented, so any run that hit the retag-then-delete branch (deletable tag sharing a digest with a preserved tag, with a disposable replacement digest available) would crash the process. Replaced the unreachable-in-practice `tagsService.Tag()` call with the wire-equivalent `ManifestService.Get` + `ManifestService.Put(WithTag(...))` sequence, matching the `docker tag` + `docker push` HTTP semantics ([#25](https://github.com/aviationexam/deckschrubber/pull/25))
  * Add integration test suite under `integration/` exercising the built binary end-to-end against an in-memory OCI v2 registry (`go-containerregistry`), covering `-latest`, `-day`/`-month`/`-year` deadlines, `-dry`, `-repo`/`-tag`/`-ntag` filters, and both Phase 1 (shared-digest retag) and Phase 2 (plain delete) deletion paths. No Docker daemon required; runs in any CI environment ([#25](https://github.com/aviationexam/deckschrubber/pull/25))
  * Fix GHCR package page showing "No description provided": GHCR reads the description from OCI manifest annotations, not Docker image config labels. `release.yml` now passes `metadata-action`'s `annotations` output to `build-push-action` and pins `DOCKER_METADATA_ANNOTATIONS_LEVELS=manifest` for single-platform builds. Takes effect on this release and onwards; existing `0.9.2` GHCR manifest is immutable and stays description-less ([#24](https://github.com/aviationexam/deckschrubber/pull/24))
* `0.9.2` (aviationexam fork):
  * Hotfix for broken `0.9.1` release workflow: pin `sigstore/cosign-installer` to the exact `v4.1.1` tag in `.github/workflows/release.yml`. `@v4` failed to resolve because the action does not publish a floating `v4` major-tag alias (only `v2` and `v3` exist), so the `v0.9.1` GHCR image was never built. No code changes versus `0.9.1` — identical binary behavior, just a release-workflow fix so the container image actually publishes.
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